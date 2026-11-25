package ir_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
)

// irSeeds are the schema samples: LLM output that decodes, validates or fails
// in the ways the backend has to handle.
var irSeeds = []string{"valid-red-dashboard.json", "valid-v11-dashboard.json", "invalid-dashboard.json"}

func readSample(f *testing.F, name string) []byte {
	f.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "schemas", "samples", name))
	if err != nil {
		f.Fatalf("read sample %s: %v", name, err)
	}
	return b
}

// FuzzDecodeStrictIR feeds arbitrary bytes, as an LLM may return them, to the
// strict decoder and the validator. An IR that is accepted must survive a
// round trip through its own JSON encoding: the backend stores and returns it
// encoded, and a later patch or import decodes it again.
func FuzzDecodeStrictIR(f *testing.F) {
	for _, name := range irSeeds {
		f.Add(readSample(f, name))
	}
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"schemaVersion":"1.1","revision":1,"panels":[{"layout":{"row":0,"col":0}}]}`))

	opts := ir.ValidateOptions{MaxPanels: ir.MaxPanelsHardCap}
	f.Fuzz(func(t *testing.T, raw []byte) {
		doc, err := ir.DecodeStrictIR(raw)
		if err != nil {
			return
		}
		if err := ir.ValidateIR(doc, opts); err != nil {
			return
		}

		encoded, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("marshal accepted IR: %v", err)
		}
		again, err := ir.DecodeStrictIR(encoded)
		if err != nil {
			t.Fatalf("accepted IR does not decode after a round trip: %v\n%s", err, encoded)
		}
		if err := ir.ValidateIR(again, opts); err != nil {
			t.Fatalf("accepted IR does not validate after a round trip: %v\n%s", err, encoded)
		}
		if !reflect.DeepEqual(doc, again) {
			t.Fatalf("round trip changed the IR:\nbefore %#v\nafter  %#v", doc, again)
		}
	})
}

// FuzzApplyPatch applies arbitrary patch JSON to a valid IR. ApplyPatch must
// leave its input untouched (undo restores it), and a patched IR must be valid
// and one revision ahead.
func FuzzApplyPatch(f *testing.F) {
	base, err := ir.DecodeStrictIR(readSample(f, "valid-v11-dashboard.json"))
	if err != nil {
		f.Fatalf("decode base IR: %v", err)
	}
	opts := ir.ValidateOptions{MaxPanels: ir.MaxPanelsHardCap}
	if err := ir.ValidateIR(base, opts); err != nil {
		f.Fatalf("validate base IR: %v", err)
	}
	baseJSON, err := json.Marshal(base)
	if err != nil {
		f.Fatalf("marshal base IR: %v", err)
	}

	seedPatch := func(ops string) []byte {
		return []byte(`{"schemaVersion":"1.1","baseRevision":` + itoa(base.Revision) + `,"summary":"edit","operations":[` + ops + `]}`)
	}
	f.Add(seedPatch(`{"op":"update_meta","title":"Renamed"}`))
	f.Add(seedPatch(`{"op":"update_meta","timeRange":{"from":"now-6h","to":"now"}}`))
	if len(base.Panels) > 0 {
		id := base.Panels[0].ID
		f.Add(seedPatch(`{"op":"update_panel","panelId":"` + id + `","panel":{"title":"New title"}}`))
		f.Add(seedPatch(`{"op":"remove_panel","panelId":"` + id + `"}`))
	}
	if len(base.Variables) > 0 {
		name := base.Variables[0].Name
		f.Add(seedPatch(`{"op":"update_variable","variableName":"` + name + `","variable":{"type":"custom","options":["a"],"defaultValue":"a"}}`))
		f.Add(seedPatch(`{"op":"remove_variable","variableName":"` + name + `"}`))
	}
	f.Add([]byte(`{"schemaVersion":"1.1","baseRevision":0,"summary":"stale","operations":[]}`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		patch, err := ir.DecodeStrictPatch(raw)
		if err != nil {
			return
		}
		input := base.Clone()
		next, _, err := ir.ApplyPatch(input, patch, opts)

		after, mErr := json.Marshal(input)
		if mErr != nil {
			t.Fatalf("marshal input after ApplyPatch: %v", mErr)
		}
		if string(after) != string(baseJSON) {
			t.Fatalf("ApplyPatch modified its input:\nbefore %s\nafter  %s", baseJSON, after)
		}
		if err != nil {
			return
		}
		if next.Revision != base.Revision+1 {
			t.Fatalf("patched revision = %d, want %d", next.Revision, base.Revision+1)
		}
		if err := ir.ValidateIR(next, opts); err != nil {
			t.Fatalf("patched IR is invalid: %v", err)
		}
	})
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
