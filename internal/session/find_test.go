package session

import (
	"testing"

	"github.com/montcao/saint/internal/store"
)

func TestFindAcceptsTheContainerName(t *testing.T) {
	st := store.New(t.TempDir())
	if _, err := st.EnsureConfig(); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSessions([]store.Session{{ID: "4jugrwdy", Name: "probe"}, {ID: "7kq2xm1p", Name: "other"}}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: st}
	for _, ref := range []string{"4jugrwdy", "saint-4jugrwdy", "4jug", "probe"} {
		got, err := svc.Find(ref)
		if err != nil || got.ID != "4jugrwdy" {
			t.Errorf("Find(%q) = %q, %v; want 4jugrwdy", ref, got.ID, err)
		}
	}
	if _, err := svc.Find("saint-"); err == nil {
		t.Error(`Find("saint-") matched; the prefix alone names no session`)
	}
}
