package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/node/config"
)

// fakeSection is a programmable Section used by tests.
type fakeSection struct {
	name string
	res  SectionResult
	err  error
}

func (f fakeSection) Name() string                              { return f.name }
func (f fakeSection) Run(_ *config.Spec) (SectionResult, error) { return f.res, f.err }

func TestInstall_SuccessShape(t *testing.T) {
	var buf bytes.Buffer
	w := &Walker{Out: &buf, Sections: DefaultInstallSections()}
	if err := w.Install(&config.Spec{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got["sections"]; !ok {
		t.Fatalf("missing sections key in %s", buf.String())
	}
	var sections map[string]map[string]interface{}
	if err := json.Unmarshal(got["sections"], &sections); err != nil {
		t.Fatalf("decode sections: %v", err)
	}
	// harden and tailscale remain stubs (T4/T5); k3s is implemented as of
	// T3 and reports reason="disabled" when its config block is absent.
	wantReasons := map[string]string{
		"harden":    "section not implemented yet",
		"tailscale": "section not implemented yet",
		"k3s":       "disabled",
	}
	for name, wantReason := range wantReasons {
		s, ok := sections[name]
		if !ok {
			t.Errorf("missing section %s", name)
			continue
		}
		if applied, _ := s["applied"].(bool); applied {
			t.Errorf("%s: expected applied=false, got %v", name, s)
		}
		if reason, _ := s["reason"].(string); reason != wantReason {
			t.Errorf("%s: reason = %q, want %q", name, reason, wantReason)
		}
	}
}

func TestInstall_ErrorShape(t *testing.T) {
	var buf bytes.Buffer
	sections := []Section{
		fakeSection{name: "harden", res: SectionResult{Applied: true, Extra: map[string]interface{}{"steps": []string{"a"}}}},
		fakeSection{name: "tailscale", res: SectionResult{Applied: true, Extra: map[string]interface{}{"version": "1.2.3"}}},
		fakeSection{name: "k3s", err: errors.New("k3s install failed: curl exit 22")},
	}
	w := &Walker{Out: &buf, Sections: sections}
	err := w.Install(&config.Spec{})
	if err == nil {
		t.Fatal("expected error from failing section")
	}
	if !strings.Contains(err.Error(), "k3s") {
		t.Errorf("error %q should mention failing section", err)
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	if got, _ := doc["error"].(string); got != "k3s install failed: curl exit 22" {
		t.Errorf("error = %q, want k3s install failed: curl exit 22", got)
	}
	if got, _ := doc["section"].(string); got != "k3s" {
		t.Errorf("section = %q, want k3s", got)
	}
	soFar, ok := doc["sections_so_far"].(map[string]interface{})
	if !ok {
		t.Fatalf("sections_so_far missing or wrong type: %v", doc["sections_so_far"])
	}
	if _, ok := soFar["harden"]; !ok {
		t.Error("sections_so_far should include harden")
	}
	if _, ok := soFar["tailscale"]; !ok {
		t.Error("sections_so_far should include tailscale")
	}
	if _, ok := soFar["k3s"]; ok {
		t.Error("sections_so_far should NOT include the failing section")
	}
}

func TestInstall_ErrorOnFirstSection(t *testing.T) {
	var buf bytes.Buffer
	sections := []Section{
		fakeSection{name: "harden", err: errors.New("boom")},
		fakeSection{name: "tailscale", res: SectionResult{Applied: true}},
	}
	w := &Walker{Out: &buf, Sections: sections}
	if err := w.Install(&config.Spec{}); err == nil {
		t.Fatal("expected error")
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	soFar, _ := doc["sections_so_far"].(map[string]interface{})
	if len(soFar) != 0 {
		t.Errorf("sections_so_far should be empty, got %v", soFar)
	}
}

func TestUninstall_FailsSoft(t *testing.T) {
	var buf bytes.Buffer
	sections := []Section{
		fakeSection{name: "k3s", err: errors.New("teardown failed")},
		fakeSection{name: "tailscale", res: SectionResult{Applied: true}},
		fakeSection{name: "harden", res: SectionResult{Applied: true}},
	}
	w := &Walker{Out: &buf, Sections: sections}
	if err := w.Uninstall(&config.Spec{}); err != nil {
		t.Fatalf("Uninstall should not return an error for per-section failures: %v", err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sectionsOut, ok := doc["sections"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing sections in %v", doc)
	}
	k3s, _ := sectionsOut["k3s"].(map[string]interface{})
	if errStr, _ := k3s["error"].(string); errStr != "teardown failed" {
		t.Errorf("k3s.error = %q, want teardown failed", errStr)
	}
	if applied, _ := k3s["applied"].(bool); applied {
		t.Errorf("k3s.applied should be false on error")
	}
	for _, name := range []string{"tailscale", "harden"} {
		s, _ := sectionsOut[name].(map[string]interface{})
		if applied, _ := s["applied"].(bool); !applied {
			t.Errorf("%s.applied = false, want true", name)
		}
	}
}

func TestUninstall_DefaultSectionsReverseOrder(t *testing.T) {
	got := DefaultUninstallSections()
	want := []string{"k3s", "tailscale", "harden"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, sec := range got {
		if sec.Name() != want[i] {
			t.Errorf("section[%d] = %q, want %q", i, sec.Name(), want[i])
		}
	}
}

func TestSectionResult_MarshalJSON(t *testing.T) {
	r := SectionResult{
		Applied: true,
		Extra:   map[string]interface{}{"version": "1.2.3"},
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["applied"] != true {
		t.Errorf("applied = %v", out["applied"])
	}
	if out["version"] != "1.2.3" {
		t.Errorf("version = %v", out["version"])
	}
	if _, present := out["reason"]; present {
		t.Error("reason should be omitted when empty")
	}
}
