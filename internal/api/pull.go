package api

// Carrying a model's metadata across from the upstream it was pulled from.
//
// A freshly pulled file is a hash and some bytes. Everything that makes it
// usable -- what it is called, what it triggers on, what it looks like, what the
// user corrected by hand three months ago -- is already sitting on the machine
// it came from, and asking Civitai for it again would mean every laptop needs a
// Civitai key and spends its own rate limit re-deriving what the NAS has known
// all along.
//
// So it is copied. The interesting question is how faithfully, and the answer
// is "including the disagreements", for a reason that is mechanical rather than
// aesthetic: see replayCandidates.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/download"
	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
)

// maxCarriedPreviews bounds how many images one pull copies, matching what an
// enrichment sweep fetches from a provider. A model can carry dozens; the first
// few are what a card and a detail panel actually show.
const maxCarriedPreviews = 4

// carryOverTimeout bounds the whole post-pull step. It runs on the download
// goroutine, so an upstream that accepts a connection and then stops talking
// must not pin it: the file is already verified and in place, and thin metadata
// is a far better outcome than a stuck queue.
const carryOverTimeout = 60 * time.Second

// afterPull records the fetch and copies over what the upstream knows.
//
// Returns a message for Job.MetaError, never for IndexError -- by the time this
// runs the file is hashed, in place and in the library, and any failure here
// leaves it that way with thinner metadata.
func (s *Server) afterPull(j download.Job) string {
	if j.UpstreamBase == "" || j.ActualSHA == "" {
		return ""
	}
	sha := strings.ToLower(j.ActualSHA)

	// Recorded first, and reported separately, because this row is what makes
	// the copy evictable later. Metadata is a convenience; without this the file
	// is stranded -- present, but with nothing saying it could be fetched again.
	var size int64
	if info, err := os.Stat(j.FinalPath); err == nil {
		size = info.Size()
	}
	if err := s.cfg.Store.PutPulledCopy(store.PulledCopy{
		SHA256: sha, Upstream: j.UpstreamBase,
		Path: j.FinalPath, Root: j.DestRoot, SizeBytes: size,
	}); err != nil {
		return "recorded the file but not where it came from: " + err.Error()
	}

	ctx, cancel := context.WithTimeout(context.Background(), carryOverTimeout)
	defer cancel()
	if err := s.carryOverMetadata(ctx, j.UpstreamBase, sha); err != nil {
		return err.Error()
	}
	return ""
}

// carryOverMetadata copies one model's metadata from an upstream.
func (s *Server) carryOverMetadata(ctx context.Context, base, sha string) error {
	// Candidates first. It is the only one of the three that can fail in a way
	// worth aborting for, since without it the model has no name.
	raw, status, err := s.upstreamGET(ctx, base+"/api/models/"+sha+"/candidates")
	if err != nil {
		return fmt.Errorf("could not read metadata from the upstream: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("the upstream returned HTTP %d for this model's metadata", status)
	}
	var views []CandidateView
	if err := json.Unmarshal(raw, &views); err != nil {
		return fmt.Errorf("could not decode the upstream's metadata: %w", err)
	}
	if err := s.replayCandidates(sha, views); err != nil {
		return err
	}

	// The rest is best-effort and collected rather than aborted on: a model with
	// its name and trigger words but no preview is usable, and failing the whole
	// carry-over over a thumbnail would throw away the part that mattered.
	var problems []string

	detail, err := s.upstreamDetail(ctx, base, sha)
	if err != nil {
		problems = append(problems, err.Error())
	} else {
		if len(detail.Tags) > 0 {
			// Scoped to this source, so it cannot delete a tag the local user
			// added by hand -- the same property handleSetTags relies on.
			if err := s.cfg.Store.SetTags(sha, provenance.SourceUpstream, detail.Tags); err != nil {
				problems = append(problems, "tags: "+err.Error())
			}
		}
		if detail.Training != nil {
			// For a self-trained LoRA this is the only metadata that exists
			// anywhere -- there is no provider to re-derive it from.
			tr := *detail.Training
			tr.SHA256 = sha
			if err := s.cfg.Store.UpsertTrainingRecord(tr); err != nil {
				problems = append(problems, "training record: "+err.Error())
			}
		}
		// Origin identity, so this daemon's own update sweep can badge a pulled
		// model without holding a provider key of its own.
		for _, o := range detail.Origins {
			o.SHA256 = sha
			if err := s.cfg.Store.PutModelOrigin(o); err != nil {
				problems = append(problems, "origin identity: "+err.Error())
				break
			}
		}
		if err := s.pullPreviews(ctx, base, sha, detail.Previews); err != nil {
			problems = append(problems, "previews: "+err.Error())
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("some metadata did not come across: %s", strings.Join(problems, "; "))
	}
	return nil
}

// replayCandidates writes the upstream's opinions under their original sources.
//
// This is the whole reason the carry-over reads /candidates rather than copying
// the resolved record, and the reason is mechanical. RecordObservations derives
// the tier from the source name, so replaying a row whose source is "manual"
// produces a local tier-3 row and one sourced "civitai" produces tier 2 --
// provenance is reproduced rather than summarised.
//
// Copying the resolved record instead would mean writing every winner under one
// source. Naming that source "civitai" asserts Civitai said things it did not;
// naming it "upstream" puts a hand-typed value at trust 1 against Civitai's 90
// in the same tier, so the next local enrichment sweep silently overwrites the
// correction the user made on the NAS. Neither is acceptable for a field the
// user has touched.
//
// The losers are replayed for a concrete reason too: they are what makes
// clearing a manual field work here the way it works there. Clear the replayed
// manual value and it falls back to the replayed Civitai value; copy only the
// winner and clearing leaves the field empty.
func (s *Server) replayCandidates(sha string, views []CandidateView) error {
	// Grouped by source because RecordObservations writes one source at a time,
	// and because that is the unit the tier is derived from.
	bySource := map[string][]store.FieldObservation{}
	add := func(field string, e candidateEntry) {
		if e.Source == "" || len(e.Value) == 0 {
			return
		}
		// The value arrives already JSON-encoded and is stored that way, so it
		// is handed through as a RawMessage rather than decoded and re-encoded.
		// A round trip would turn 1 into 1.0 and reorder object keys, which
		// valuesEqual tolerates but which would make every carried value look
		// like a fresh disagreement.
		bySource[e.Source] = append(bySource[e.Source],
			store.FieldObservation{Field: field, Value: json.RawMessage(e.Value)})
	}

	for _, v := range views {
		add(v.Field, v.Winner)
		for _, l := range v.Losers {
			add(v.Field, l)
		}
	}

	for source, obs := range bySource {
		// The wire carries a tier alongside each entry and it is deliberately
		// ignored. Trusting it would let a remote daemon assert tier 3 for a
		// source this build classifies as a tool scrape; deriving it locally
		// means an unrecognised source from a newer upstream lands in TierTool,
		// which is the documented safe default and degrades downward.
		if err := s.cfg.Store.RecordObservations(sha, source, obs); err != nil {
			return fmt.Errorf("recording %s metadata: %w", source, err)
		}
	}
	if len(bySource) == 0 {
		return nil
	}
	// Materialise, so the model appears in the library with its name rather than
	// as a bare hash until something else happens to resolve it.
	if _, err := s.cfg.Store.ResolveModel(sha); err != nil {
		return fmt.Errorf("resolving carried metadata: %w", err)
	}
	return nil
}

// pullPreviews copies preview images into the local blob store.
func (s *Server) pullPreviews(ctx context.Context, base, sha string, previews []store.PreviewImage) error {
	if s.cfg.Blobs == nil {
		return nil
	}
	var problems []string
	copied := 0

	for _, p := range previews {
		if copied >= maxCarriedPreviews {
			break
		}
		if p.ImageSHA256 == "" {
			continue
		}
		// Content addressing makes the skip exact: if these bytes are already
		// here, they are the same bytes.
		if _, err := os.Stat(s.cfg.Blobs.Path(p.ImageSHA256)); err == nil {
			continue
		}

		data, err := s.fetchBlob(ctx, base+"/api/previews/"+p.ImageSHA256)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !blobstore.IsImage(data) {
			problems = append(problems, p.ImageSHA256+" was not an image")
			continue
		}

		// Source and position are preserved rather than reassigned: a preview
		// the user chose by hand on the NAS stays manual here, which is what
		// AddPreviewImage's own precedence already protects.
		source := p.Source
		if source == "" {
			source = provenance.SourceUpstream
		}
		stored, err := s.storePreviewAt(sha, data, source, p.Position)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		// The blob store hashes what it writes, so this is a free integrity
		// check on a transfer that had none: the upstream addressed these bytes
		// by hash and they must still hash to it.
		if stored != nil && !strings.EqualFold(stored.ImageSHA256, p.ImageSHA256) {
			problems = append(problems, fmt.Sprintf(
				"%s arrived hashing as %s", p.ImageSHA256, stored.ImageSHA256))
			_ = s.cfg.Store.RemovePreviewImage(sha, stored.ImageSHA256)
			continue
		}
		copied++
	}

	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// upstreamDetail reads one model's full record from the upstream.
func (s *Server) upstreamDetail(ctx context.Context, base, sha string) (*ModelDetail, error) {
	raw, status, err := s.upstreamGET(ctx, base+"/api/models/"+sha)
	if err != nil {
		return nil, fmt.Errorf("model detail: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("model detail: the upstream returned HTTP %d", status)
	}
	var detail ModelDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil, fmt.Errorf("model detail: %w", err)
	}
	return &detail, nil
}

// fetchBlob reads one preview image from the upstream.
func (s *Server) fetchBlob(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	s.authorizeUpstream(req)

	resp, err := s.outboundClient(30 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d", target, resp.StatusCode)
	}
	// Capped at what the blob store would accept anyway, so a hostile or broken
	// upstream cannot make this allocate without bound.
	return io.ReadAll(io.LimitReader(resp.Body, blobstore.MaxBlobBytes))
}
