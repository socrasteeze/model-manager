// Package basemodel owns the vocabulary of base-model families.
//
// It exists for the same reason internal/modeltype does: the name was being
// derived in several places that disagreed. `internal/ingest` knew about Flux
// and nothing about Anima or Krea; `internal/interpret` knew about "Anima 2B"
// and "Krea 2" and nothing about them being related to anything else. A model
// could therefore land in one bucket when identified from a sidecar and another
// when identified from its path, which makes a base-model filter quietly lie.
//
// It matters twice over now, because the family also decides which ComfyUI
// workflow can render a preview. An Illustrious lora and a FLUX.2 lora need
// different graphs -- different loaders, different text encoders, a different
// VAE -- so "which family is this" stopped being a display detail the moment
// thumbnails could be generated.
//
// # Why this set is open and the model-type set is closed
//
// An unrecognised *type* normalizes to "" because it decides a directory name,
// and inventing a folder from an unvalidated string is how files end up
// somewhere no tool will look.
//
// An unrecognised *base model* is kept verbatim, because the set is genuinely
// open-ended: a new architecture ships every few months, and dropping it would
// erase the one field that says what a file can even be used with. A name this
// package does not know is a name it has not learned yet, not a name that is
// wrong.
package basemodel

import (
	"regexp"
	"strings"
)

// The families this app can name.
const (
	SD15        = "SD 1.5"
	SD2         = "SD 2.x"
	SD3         = "SD 3"
	SDXL        = "SDXL"
	Pony        = "Pony"
	Illustrious = "Illustrious"
	NoobAI      = "NoobAI"
	Anima       = "Anima"
	Flux1       = "Flux.1"
	Flux2       = "Flux.2"
	Krea        = "Krea 2"
	Qwen        = "Qwen"
	Wan         = "Wan"
	Hunyuan     = "Hunyuan"
)

// Known lists every family, in rough "what people actually hold" order. Used to
// seed the settings UI; it is not a closed set, and a library will contain
// names that are not here.
var Known = []string{
	SDXL, Illustrious, Pony, NoobAI, Anima,
	Flux2, Flux1, Krea,
	SD15, SD2, SD3,
	Qwen, Wan, Hunyuan,
}

// patterns are tried in order, and the order is load-bearing.
//
// Flux.2 and Krea must be tested before Flux.1: both are commonly labelled with
// "flux" somewhere in the name, and a `flux` match that ran first would collapse
// three families -- which need three different ComfyUI graphs -- into one.
// Likewise the SDXL derivatives are tested before SDXL itself, because an
// Illustrious model is very often also tagged "sdxl", and the derivative is the
// more specific true statement.
var patterns = []struct {
	re     *regexp.Regexp
	family string
}{
	// SDXL derivatives, most specific first.
	//
	// A trailing \w* where the family token is distinctive enough to carry one:
	// real filenames say `illustriousXL`, `noobaiXL`, `krea2`, and requiring a
	// word boundary after the family name would miss every one of them.
	{regexp.MustCompile(`(?i)\billustrious\w*`), Illustrious},
	{regexp.MustCompile(`(?i)\bnoob\s*ai\w*`), NoobAI},
	// `pony` stays strict about the bare word: a lora called "ponytail" is
	// about hair, not Pony Diffusion, and this is the one family token that is
	// also an English word. A version suffix like "PonyV6" is unambiguous.
	{regexp.MustCompile(`(?i)\bpony\b|\bponyxl\b|\bpdxl\b|\bpony\s*v\d+\b`), Pony},

	// Flux family and derivatives, newest and most specific first.
	{regexp.MustCompile(`(?i)\bkrea\d*\b`), Krea},
	{regexp.MustCompile(`(?i)\bflux\s*2\b|\bflux2\b|\bklein\b`), Flux2},
	// Trailing \w*, like illustrious/noobai above: "FluxDev" and "Flux1" are
	// glued spellings a bare `\bflux\b` does not reach.
	{regexp.MustCompile(`(?i)\bflux\w*`), Flux1},

	// `anima` also stays strict, for the same reason as pony and more so:
	// `\banima\w*` would swallow "animal" and "animation", and a lora named
	// "animal print" is not an Anima model.
	{regexp.MustCompile(`(?i)\banima\b|\banima\s*2b\b|\banima2b\b`), Anima},

	{regexp.MustCompile(`(?i)\bsdxl\b|\bsd\s*xl\b|^xl$|\bxl\b`), SDXL},
	{regexp.MustCompile(`(?i)\bsd\s*3\b|\bsd3\b`), SD3},
	// \s* rather than \.? between the two digits: separators -- including a
	// literal dot -- are already flattened to spaces below before these
	// patterns run, so "sd 1.5" arrives here as "sd 1 5".
	{regexp.MustCompile(`(?i)\bsd\s*1\s*5\b|\bsd1\b`), SD15},
	{regexp.MustCompile(`(?i)\bsd\s*2\b|\bsd2\b`), SD2},

	{regexp.MustCompile(`(?i)\bqwen\b`), Qwen},
	// Trailing \d*: "Wan2.1" and "Wan2.2" are the spellings people actually
	// use, and a bare `\bwan\b` does not reach past the glued version number.
	{regexp.MustCompile(`(?i)\bwan\d*\b`), Wan},
	{regexp.MustCompile(`(?i)\bhunyuan\w*`), Hunyuan},
}

// Parent returns the architecture a family renders with when it has no
// workflow or checkpoint configuration of its own -- an SDXL derivative like
// Illustrious needs the same graph shape as SDXL, and Krea is Flux.1-derived.
// "" means family is not a derivative of a more commonly-configured one.
func Parent(family string) string {
	switch family {
	case Pony, Illustrious, NoobAI, Anima:
		return SDXL
	case Krea:
		return Flux1
	}
	return ""
}

// Normalize collapses the many spellings of one family onto a single name.
//
// An unrecognised value comes back trimmed but otherwise untouched. See the
// package comment: this set is open on purpose.
func Normalize(v string) string {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return ""
	}
	if f := Match(trimmed); f != "" {
		return f
	}
	return trimmed
}

// Match returns the family a string names, or "" if none is recognised.
//
// Separate from Normalize because a caller scanning a *path* wants to know
// whether a segment said anything at all -- "" means "this told me nothing",
// where Normalize would hand back the segment as if it were a family name.
func Match(v string) string {
	// Separators become spaces so the word boundaries in the patterns work on
	// `flux.2-klein_dev` the same way they do on `flux 2 klein dev`.
	s := strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', '.', '/', '\\', '(', ')', '[', ']', '+':
			return ' '
		}
		return r
	}, v)
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	for _, p := range patterns {
		if p.re.MatchString(s) {
			return p.family
		}
	}
	return ""
}

// IsKnown reports whether a name is one this app can name.
func IsKnown(v string) bool {
	for _, f := range Known {
		if f == v {
			return true
		}
	}
	return false
}
