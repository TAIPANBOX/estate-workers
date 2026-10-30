package models

import "testing"

func TestFableAndMythosAreRefusedWhateverTheSpelling(t *testing.T) {
	for _, m := range []string{"claude-fable-5-1", "claude-fable-5", "Claude-FABLE-5-1", "claude-mythos-5-1", "x-mythos"} {
		if err := Check(m, "a reason that is long enough"); err == nil {
			t.Errorf("Check(%q) = nil, want a refusal", m)
		}
	}
}

func TestFableStaysRefusedEvenIfSomeoneAddsItToTheKnownList(t *testing.T) {
	saved := Known
	defer func() { Known = saved }()
	Known = append(append([]string{}, saved...), "claude-fable-5-1", "claude-mythos-5-1")
	for _, m := range []string{"claude-fable-5-1", "claude-mythos-5-1"} {
		if err := Check(m, "a reason that is long enough"); err == nil {
			t.Errorf("Check(%q) = nil with %q in Known", m, m)
		}
	}
}

func TestUnknownModelIsRefused(t *testing.T) {
	for _, m := range []string{"", "claude-sonnet-5", "claude-opus-5", "gpt-6", "claude-haiku-5-5 "} {
		if err := Check(m, ""); err == nil {
			t.Errorf("Check(%q) = nil, want a refusal", m)
		}
	}
}

func TestOpusNeedsAReason(t *testing.T) {
	if err := Check(Opus, ""); err == nil {
		t.Error("Opus without a reason was accepted")
	}
	if err := Check(Opus, "  short  "); err == nil {
		t.Error("Opus with a reason under the minimum was accepted")
	}
	if err := Check(Opus, "dossier rewrite needs judgment"); err != nil {
		t.Errorf("Opus with a reason refused: %v", err)
	}
}

func TestHaikuAndSonnetNeedNoReason(t *testing.T) {
	for _, m := range []string{Haiku, Sonnet} {
		if err := Check(m, ""); err != nil {
			t.Errorf("Check(%q) = %v", m, err)
		}
	}
}

func TestEveryKnownModelHasAHardCapAndAName(t *testing.T) {
	for _, m := range Known {
		if HardMaxRunUSD[m] <= 0 {
			t.Errorf("%s has no hard run cap", m)
		}
		if DisplayName(m) == "Claude" {
			t.Errorf("%s has no display name", m)
		}
	}
	if DisplayName("other") != "Claude" {
		t.Error("unknown model should fall back to Claude")
	}
	if !(HardMaxRunUSD[Haiku] < HardMaxRunUSD[Sonnet] && HardMaxRunUSD[Sonnet] < HardMaxRunUSD[Opus]) {
		t.Error("hard caps should rise with the price of the model")
	}
}
