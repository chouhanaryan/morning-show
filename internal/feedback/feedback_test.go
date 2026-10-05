package feedback

import (
	"path/filepath"
	"testing"
)

func TestEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.AddInterest("cloud networking") || s.AddInterest("  Cloud Networking ") {
		t.Error("AddInterest should add once and dedupe case-insensitively")
	}
	if !s.AddCorrection("less crypto") || !s.AddCorrection("more VPC") {
		t.Error("AddCorrection failed")
	}
	if !s.RemoveCorrection("LESS CRYPTO") || s.RemoveCorrection("absent") {
		t.Error("RemoveCorrection should match case-insensitively and report misses")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := s2.Snapshot()
	if len(p.StandingInterests) != 1 || len(p.ActiveCorrections) != 1 || p.ActiveCorrections[0] != "more VPC" {
		t.Errorf("round trip = %+v", p)
	}
	if !s2.ClearCorrections() || s2.ClearCorrections() {
		t.Error("ClearCorrections should report change only once")
	}
	if !s2.RemoveInterest("cloud networking") || len(s2.Snapshot().StandingInterests) != 0 {
		t.Error("RemoveInterest failed")
	}
}
