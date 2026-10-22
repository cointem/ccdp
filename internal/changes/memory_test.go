package changes

import (
	"testing"
)

func TestMemoryClonePreservesObjectsAndIsolatesTransactions(t *testing.T) {
	store := New("")
	file, err := store.ImportFile([]byte("saved content"), 0600, "regular")
	if err != nil {
		t.Fatal(err)
	}
	valueID, err := store.PutValue(map[string]string{"key": "value"})
	if err != nil {
		t.Fatal(err)
	}
	plan := Plan{ID: digest([]byte("plan"))}
	if err := store.save("plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	if err := store.save("transactions", plan.ID, Journal{Plan: plan, Status: "applying"}); err != nil {
		t.Fatal(err)
	}
	snapshot := store.Clone()
	resumed := snapshot.Clone()
	if err := resumed.save("transactions", plan.ID, Journal{Plan: plan, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]*Store{"original": store, "snapshot": snapshot, "resumed": resumed} {
		data, err := source.Content(file)
		if err != nil || string(data) != "saved content" {
			t.Fatalf("%s content=%q, %v", name, data, err)
		}
		var value map[string]string
		if err := source.Value(valueID, &value); err != nil || value["key"] != "value" {
			t.Fatalf("%s value=%v, %v", name, value, err)
		}
		journal, err := source.Journal(plan.ID)
		want := "applying"
		if name == "resumed" {
			want = "completed"
		}
		if err != nil || journal.Status != want {
			t.Fatalf("%s transaction=%+v, %v", name, journal, err)
		}
	}
}
