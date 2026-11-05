package sandbox

import (
	"testing"
)

func TestParseListJSON(t *testing.T) {
	t.Parallel()

	data := []byte(`[
	  {
	    "id": "saint-test",
	    "configuration": {
	      "labels": {"saint.runtime": "claude"},
	      "image": {"reference": "saint/claude:latest"},
	      "mounts": [
	        {"destination": "/repos/app", "source": "/repos/app", "options": []},
	        {"destination": "/saint", "source": "/tmp/saint-test/.saint/sessions/test", "options": []}
	      ]
	    },
	    "status": {"state": "running"}
	  }
	]`)

	infos, err := ParseListJSON(data)
	if err != nil {
		t.Fatalf("ParseListJSON() error = %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("len(infos) = %d, want 1", len(infos))
	}
	if infos[0].Name != "saint-test" {
		t.Fatalf("Name = %q, want saint-test", infos[0].Name)
	}
	if infos[0].Runtime != RuntimeClaude {
		t.Fatalf("Runtime = %q, want %q", infos[0].Runtime, RuntimeClaude)
	}
	if infos[0].State != StateRunning {
		t.Fatalf("State = %v, want %v", infos[0].State, StateRunning)
	}
	if len(infos[0].Paths) != 1 || infos[0].Paths[0] != "/repos/app" {
		t.Fatalf("Paths = %#v", infos[0].Paths)
	}
}

func TestParseListJSONEmpty(t *testing.T) {
	t.Parallel()

	infos, err := ParseListJSON([]byte("[]"))
	if err != nil {
		t.Fatalf("ParseListJSON() error = %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("len(infos) = %d, want 0", len(infos))
	}
}
