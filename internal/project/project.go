// Package project writes master metadata back out as tool sidecars.
//
// Consumer sidecars are derived artifacts (spec §4). Master is projected into
// each tool's dialect, and if Stability Matrix or SwarmUI mangles one, it is
// regenerated. Their "pull metadata" button stops being a threat and becomes a
// no-op overwritten on the next projection.
//
// This is push, not pull (§18): nothing supports pull today.
package project

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/socrasteeze/model-manager/internal/basemodel"
	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/store"
)

// Target is a tool dialect.
type Target string

const (
	TargetStabilityMatrix Target = "stability-matrix"
	TargetA1111           Target = "a1111"
	TargetSwarmUI         Target = "swarmui"
	TargetLoRAManager     Target = "lora-manager"
)

// AllTargets is every dialect that can be written.
var AllTargets = []Target{TargetStabilityMatrix, TargetA1111, TargetSwarmUI, TargetLoRAManager}

// Options configures a projection run.
type Options struct {
	// Targets to write. Empty means none -- §15 says start with one tool, verify,
	// then add others, so there is deliberately no "all" default.
	Targets []Target

	// Blobs supplies preview images. Optional.
	Blobs *blobstore.Store

	// WritePreviews emits a preview image beside each model.
	WritePreviews bool

	// DryRun reports what would be written without touching the disk.
	DryRun bool

	// Overwrite replaces a sidecar this app did not write. Off by default: a
	// file another tool authored may hold something master never captured, and
	// silently replacing it would be exactly the destructive stomp this project
	// exists to stop.
	Overwrite bool

	// OnlySHA restricts the run to one model, for verifying a dialect before
	// letting it loose on a library.
	OnlySHA string

	Logf func(format string, args ...any)
}

// Stats summarizes a run.
type Stats struct {
	ModelsConsidered int
	Written          int
	Skipped          int
	Previews         int
	Errors           int
	ByTarget         map[Target]int
	Elapsed          time.Duration
}

// Run projects master into the requested tool dialects.
func Run(ctx context.Context, st *store.Store, opts Options) (*Stats, error) {
	started := time.Now()
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if len(opts.Targets) == 0 {
		return nil, fmt.Errorf(
			"project: no targets given. Start with one tool and verify it before adding others")
	}
	stats := &Stats{ByTarget: map[Target]int{}}

	paths, err := projectionTargets(st, opts.OnlySHA)
	if err != nil {
		return nil, err
	}

	for _, p := range paths {
		if ctx.Err() != nil {
			break
		}
		stats.ModelsConsidered++

		rec, err := st.GetModelRecord(p.SHA256)
		if err != nil {
			stats.Errors++
			continue
		}
		tags, err := st.Tags(p.SHA256)
		if err != nil {
			stats.Errors++
			continue
		}
		training, _ := st.GetTrainingRecord(p.SHA256)

		// Nothing worth saying about this model yet. Writing a sidecar anyway
		// would be worse than writing none: it would carry only a filename and a
		// hash, and a tool would read that as authoritative emptiness and stop
		// showing whatever it had worked out for itself.
		//
		// A non-nil record is not the test -- resolution writes a row for every
		// model, whether or not any source had an opinion.
		if !worthWriting(rec, tags, training) {
			stats.Skipped++
			continue
		}

		for _, target := range opts.Targets {
			body, filename, err := render(target, p, rec, tags, training)
			if err != nil {
				stats.Errors++
				logf("%s: %v", p.Path, err)
				continue
			}
			if body == nil {
				continue
			}

			dest := filepath.Join(filepath.Dir(p.Path), filename)
			if !opts.Overwrite && !ourSidecar(dest) && exists(dest) {
				// Somebody else's file. Left alone.
				stats.Skipped++
				continue
			}
			if opts.DryRun {
				stats.Written++
				stats.ByTarget[target]++
				continue
			}
			if err := writeAtomic(dest, body); err != nil {
				stats.Errors++
				logf("%s: %v", dest, err)
				continue
			}
			stats.Written++
			stats.ByTarget[target]++
		}

		if opts.WritePreviews && opts.Blobs != nil && !opts.DryRun {
			if n := writePreview(st, opts.Blobs, p); n > 0 {
				stats.Previews += n
			}
		}
	}

	stats.Elapsed = time.Since(started)
	return stats, nil
}

// worthWriting reports whether master knows anything a tool would want.
func worthWriting(rec *store.ModelRecord, tags []string, training *store.TrainingRecord) bool {
	if rec == nil {
		return false
	}
	if rec.Name != "" || rec.Description != "" || rec.BaseModel != "" ||
		rec.Type != "" || rec.Version != "" ||
		len(rec.TriggerWords) > 0 || rec.RecommendedWeight != nil || rec.NSFW != nil {
		return true
	}
	if len(tags) > 0 {
		return true
	}
	return training != nil
}

// generatorMarker identifies a sidecar this app wrote.
//
// Without it, projection could not tell its own output from a file another tool
// authored, and would have to choose between never overwriting (making
// regeneration useless) and always overwriting (destroying data master never
// captured). The marker makes the distinction, so regeneration is safe and
// foreign files are left alone.
const generatorMarker = "model-manager"

const markerKey = "_generated_by"

func ourSidecar(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if len(data) > 4<<20 {
		return false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	raw, ok := probe[markerKey]
	if !ok {
		return false
	}
	var who string
	if err := json.Unmarshal(raw, &who); err != nil {
		return false
	}
	return who == generatorMarker
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeAtomic writes via a temporary file and a rename, so a tool reading
// concurrently never sees a half-written sidecar -- which is precisely the
// disconnected-metadata symptom this project exists to eliminate.
func writeAtomic(path string, body []byte) error {
	tmp := path + ".mm-tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("project: writing %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("project: publishing %s: %w", path, err)
	}
	return nil
}

// render produces one tool's sidecar.
func render(target Target, p store.FilePath, rec *store.ModelRecord,
	tags []string, training *store.TrainingRecord) ([]byte, string, error) {

	stem := strings.TrimSuffix(filepath.Base(p.Path), filepath.Ext(p.Path))

	switch target {
	case TargetStabilityMatrix:
		body := map[string]any{
			markerKey:          generatorMarker,
			"ModelName":        rec.Name,
			"ModelDescription": rec.Description,
			"VersionName":      rec.Version,
			"BaseModel":        rec.BaseModel,
			"ModelType":        toStabilityMatrixType(rec.Type),
			"TrainedWords":     nonNil(rec.TriggerWords),
			"Tags":             nonNil(tags),
			"FileName":         filepath.Base(p.Path),
			"Hashes":           map[string]string{"SHA256": strings.ToUpper(p.SHA256)},
		}
		if rec.NSFW != nil {
			// This dialect asks the question the other way round.
			body["IsSfw"] = !*rec.NSFW
			body["Nsfw"] = *rec.NSFW
		}
		out, err := marshal(body)
		return out, stem + ".cm-info.json", err

	case TargetA1111:
		body := map[string]any{
			markerKey:         generatorMarker,
			"description":     rec.Description,
			"sd version":      rec.BaseModel,
			"activation text": strings.Join(rec.TriggerWords, ", "),
			"notes":           notesFor(rec, training),
		}
		if rec.RecommendedWeight != nil {
			body["preferred weight"] = *rec.RecommendedWeight
		}
		out, err := marshal(body)
		return out, stem + ".json", err

	case TargetSwarmUI:
		body := map[string]any{
			markerKey:        generatorMarker,
			"model_name":     rec.Name,
			"title":          rec.Name,
			"description":    rec.Description,
			"trigger_phrase": strings.Join(rec.TriggerWords, ", "),
			"tags":           nonNil(tags),
			"architecture":   toSwarmArchitecture(rec.BaseModel),
			"usage_hint":     usageHint(rec),
		}
		out, err := marshal(body)
		return out, stem + ".json", err

	case TargetLoRAManager:
		body := map[string]any{
			markerKey:    generatorMarker,
			"model_name": rec.Name,
			"file_name":  stem,
			"base_model": rec.BaseModel,
			"tags":       nonNil(tags),
			"notes":      notesFor(rec, training),
			"sha256":     p.SHA256,
		}
		if rec.RecommendedWeight != nil {
			tips, _ := json.Marshal(map[string]any{"strength": *rec.RecommendedWeight})
			body["usage_tips"] = string(tips)
		}
		out, err := marshal(body)
		return out, stem + ".metadata.json", err
	}

	return nil, "", fmt.Errorf("project: unknown target %q", target)
}

// SwarmUI and A1111 both claim <stem>.json, so projecting both into one
// directory would have the second silently overwrite the first. Detect that at
// the call site rather than discovering it as missing metadata later.
func ConflictingTargets(targets []Target) []Target {
	var swarm, a1111 bool
	for _, t := range targets {
		switch t {
		case TargetSwarmUI:
			swarm = true
		case TargetA1111:
			a1111 = true
		}
	}
	if swarm && a1111 {
		return []Target{TargetSwarmUI, TargetA1111}
	}
	return nil
}

func marshal(body map[string]any) ([]byte, error) {
	// Drop empty values rather than writing nulls: a tool reading a null may
	// treat it as an authoritative "no value" and display nothing where it would
	// otherwise have shown its own.
	for k, v := range body {
		switch t := v.(type) {
		case string:
			if strings.TrimSpace(t) == "" {
				delete(body, k)
			}
		case []string:
			if len(t) == 0 {
				delete(body, k)
			}
		case nil:
			delete(body, k)
		}
	}
	out, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func notesFor(rec *store.ModelRecord, training *store.TrainingRecord) string {
	var parts []string
	if training != nil {
		if training.Trainer != "" {
			parts = append(parts, "trained with "+training.Trainer)
		}
		if training.Dataset != "" {
			parts = append(parts, "dataset: "+training.Dataset)
		}
		if training.Notes != "" {
			parts = append(parts, training.Notes)
		}
	}
	if rec.Origin == "self-trained" && len(parts) == 0 {
		parts = append(parts, "self-trained")
	}
	return strings.Join(parts, "\n")
}

func usageHint(rec *store.ModelRecord) string {
	if rec.RecommendedWeight == nil {
		return ""
	}
	return fmt.Sprintf("recommended weight %.2f", *rec.RecommendedWeight)
}

func toStabilityMatrixType(t string) string {
	switch t {
	case "lora":
		return "Lora"
	case "lycoris":
		return "LoCon"
	case "checkpoint":
		return "Checkpoint"
	case "embedding":
		return "TextualInversion"
	case "vae":
		return "VAE"
	case "controlnet":
		return "Controlnet"
	case "upscaler":
		return "Upscaler"
	}
	return ""
}

func toSwarmArchitecture(base string) string {
	switch base {
	// Anima is grouped with the SDXL derivatives here because SwarmUI's
	// architecture list has no entry of its own for it, and claiming an
	// architecture SwarmUI does not know would be worse than the nearest true
	// statement about what loads it.
	case basemodel.SDXL, basemodel.Pony, basemodel.Illustrious,
		basemodel.NoobAI, basemodel.Anima:
		return "stable-diffusion-xl-v1-base"
	case basemodel.SD15:
		return "stable-diffusion-v1"
	case basemodel.SD2:
		return "stable-diffusion-v2"
	case basemodel.SD3:
		return "stable-diffusion-v3-medium"
	case basemodel.Flux1, basemodel.Krea:
		// Krea is Flux.1-derived, and SwarmUI has no separate label for it.
		return "Flux.1-dev"
	case basemodel.Flux2:
		return "Flux.2-dev"
	}
	// An architecture this app cannot name for SwarmUI is left unset rather
	// than guessed: a wrong architecture makes SwarmUI load the model with the
	// wrong pipeline, which is worse than SwarmUI not knowing.
	return ""
}

func writePreview(st *store.Store, blobs *blobstore.Store, p store.FilePath) int {
	preview, err := st.PrimaryPreview(p.SHA256)
	if err != nil || preview == nil {
		return 0
	}
	data, err := blobs.Read(preview.ImageSHA256)
	if err != nil {
		return 0
	}

	ext := ".png"
	switch preview.MIME {
	case "image/jpeg":
		ext = ".jpeg"
	case "image/webp":
		ext = ".webp"
	}
	stem := strings.TrimSuffix(filepath.Base(p.Path), filepath.Ext(p.Path))
	dest := filepath.Join(filepath.Dir(p.Path), stem+".preview"+ext)

	// A preview beside a model is not JSON, so the generator marker cannot live
	// inside it. Never replacing an existing one is the conservative choice:
	// the user may have chosen that image deliberately.
	if exists(dest) {
		return 0
	}
	if err := writeAtomic(dest, data); err != nil {
		return 0
	}
	return 1
}

// projectionTargets lists the paths worth writing beside.
//
// Present and confirmed only. Writing a sidecar beside a provisional path would
// attach master's opinion to a file whose identity is a guess -- §10.1 bars a
// probe-bound path from projection by name.
func projectionTargets(st *store.Store, onlySHA string) ([]store.FilePath, error) {
	query := `
        SELECT id, sha256, path, root, device, inode, size, mtime_ns
          FROM model_file_path
         WHERE present = 1 AND provisional = 0`
	args := []any{}
	if onlySHA != "" {
		query += ` AND sha256 = ?`
		args = append(args, onlySHA)
	}
	query += ` ORDER BY path`

	rows, err := st.DB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("project: selecting targets: %w", err)
	}
	defer rows.Close()

	var out []store.FilePath
	for rows.Next() {
		var p store.FilePath
		var device, inode int64
		if err := rows.Scan(&p.ID, &p.SHA256, &p.Path, &p.Root,
			&device, &inode, &p.Size, &p.MtimeNs); err != nil {
			return nil, err
		}
		p.Device, p.Inode = uint64(device), uint64(inode)
		p.Present = true
		out = append(out, p)
	}
	return out, rows.Err()
}

// Summary renders the stats for the CLI.
func (s *Stats) Summary() string {
	out := fmt.Sprintf("considered %d model path(s), wrote %d sidecar(s)",
		s.ModelsConsidered, s.Written)
	if s.Previews > 0 {
		out += fmt.Sprintf(", %d preview(s)", s.Previews)
	}
	if s.Skipped > 0 {
		out += fmt.Sprintf("\nskipped %d (no metadata yet, or a sidecar this app did not write)", s.Skipped)
	}
	for target, n := range s.ByTarget {
		out += fmt.Sprintf("\n  %-18s %d", target, n)
	}
	if s.Errors > 0 {
		out += fmt.Sprintf("\n%d error(s)", s.Errors)
	}
	return out + fmt.Sprintf("\n(%s)", s.Elapsed.Round(time.Millisecond))
}
