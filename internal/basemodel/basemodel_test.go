package basemodel

import "testing"

// The four families this library is actually made of. Getting these wrong is
// not cosmetic: the family decides which ComfyUI graph can render a preview,
// and three of these four need different loaders.
func TestTheFamiliesThisLibraryHolds(t *testing.T) {
	cases := map[string]string{
		"SDXL 1.0":           SDXL,
		"sdxl":               SDXL,
		"Illustrious":        Illustrious,
		"illustriousXL v2.0": Illustrious,
		"Anima":              Anima,
		"anima_2b":           Anima,
		"FLUX.2 [klein]":     Flux2,
		"flux2-klein":        Flux2,
		"klein":              Flux2,
		"Krea 2":             Krea,
		"FLUX.1 Krea [dev]":  Krea,
		"krea2":              Krea,
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// Order is load-bearing. Both Flux.2 and Krea are commonly labelled with "flux"
// somewhere, and a `flux` match running first would collapse three families --
// which need three different graphs -- into one.
func TestFluxDerivativesAreNotCollapsedIntoFlux1(t *testing.T) {
	if got := Normalize("FLUX.2 [klein] dev"); got != Flux2 {
		t.Errorf("flux.2 klein = %q, want %q", got, Flux2)
	}
	if got := Normalize("flux1-krea-dev.safetensors"); got != Krea {
		t.Errorf("flux krea = %q, want %q", got, Krea)
	}
	if got := Normalize("Flux.1 D"); got != Flux1 {
		t.Errorf("flux.1 = %q, want %q", got, Flux1)
	}
}

// An Illustrious model is very often also tagged SDXL. The derivative is the
// more specific true statement, and it is the one the user filters by.
func TestSDXLDerivativesWinOverPlainSDXL(t *testing.T) {
	for in, want := range map[string]string{
		"Illustrious XL":    Illustrious,
		"Pony Diffusion XL": Pony,
		"NoobAI-XL":         NoobAI,
		"SDXL Turbo":        SDXL,
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// Separators must not hide a family name: `flux.2-klein_dev` has to read the
// same as `flux 2 klein dev`.
func TestSeparatorsDoNotHideTheFamily(t *testing.T) {
	for _, in := range []string{
		"flux.2-klein", "flux_2_klein", "[FLUX.2]-klein(dev)", "flux2/klein",
	} {
		if got := Normalize(in); got != Flux2 {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, Flux2)
		}
	}
}

// Unlike a model type, an unrecognised base model is kept verbatim. The set is
// genuinely open -- a new architecture ships every few months -- and dropping
// it would erase the one field that says what a file can be used with.
func TestUnknownFamilyIsKeptVerbatim(t *testing.T) {
	for _, in := range []string{"Chroma", "HiDream", "Something Released Tomorrow"} {
		if got := Normalize(in); got != in {
			t.Errorf("Normalize(%q) = %q; an unknown family must not be dropped", in, got)
		}
		// But Match still says "I do not recognise this", which is what a path
		// scanner needs so it does not treat a random directory name as a
		// family.
		if got := Match(in); got != "" {
			t.Errorf("Match(%q) = %q, want \"\"", in, got)
		}
	}
	if got := Normalize("   "); got != "" {
		t.Errorf("Normalize(blank) = %q", got)
	}
}

// Match is the "did this tell me anything" question, which is what separates it
// from Normalize.
func TestMatchDistinguishesSilenceFromAName(t *testing.T) {
	if got := Match("loras"); got != "" {
		t.Errorf("a plain folder name matched %q", got)
	}
	if got := Match("Illustrious"); got != Illustrious {
		t.Errorf("Match(Illustrious) = %q", got)
	}
}

func TestKnownListIsSelfConsistent(t *testing.T) {
	for _, f := range Known {
		if !IsKnown(f) {
			t.Errorf("IsKnown(%q) = false for a listed family", f)
		}
		if got := Normalize(f); got != f {
			t.Errorf("Normalize(%q) = %q; a family name must normalize to itself", f, got)
		}
	}
}

// `pony` and `anima` are also ordinary English words, so their patterns stay
// strict where the others take a suffix. A lora about hair or about wildlife
// must not be bucketed as a base-model family.
func TestEverydayWordsAreNotMistakenForFamilies(t *testing.T) {
	for _, in := range []string{
		"ponytail hairstyle", "animal print", "animation style",
		"kreative lettering",
	} {
		if got := Match(in); got != "" {
			t.Errorf("Match(%q) = %q; that is an ordinary word, not a family", in, got)
		}
	}
	// While the real thing still matches.
	if got := Match("ponyXL v6"); got != Pony {
		t.Errorf("Match(ponyXL) = %q, want %q", got, Pony)
	}
	if got := Match("anima 2b"); got != Anima {
		t.Errorf("Match(anima 2b) = %q, want %q", got, Anima)
	}
}
