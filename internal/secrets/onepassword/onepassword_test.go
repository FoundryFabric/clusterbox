package onepassword_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/secrets/onepassword"
)

func makeRunner(fn func(args []string) ([]byte, error)) func(ctx context.Context, env []string, args ...string) ([]byte, error) {
	return func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		return fn(args)
	}
}

var sampleItemJSON []byte

func init() {
	var err error
	sampleItemJSON, err = json.Marshal(map[string]interface{}{
		"id":    "item-1",
		"title": "k3d",
		"fields": []map[string]interface{}{
			{"label": "GRAFANA_ADMIN_PASSWORD", "value": "grafana-secret", "purpose": ""},
			{"label": "DB_PASSWORD", "value": "db-secret", "purpose": ""},
			{"label": "username", "value": "admin", "purpose": "USERNAME"}, // filtered
		},
	})
	if err != nil {
		panic(err)
	}
}

// TestGetAll returns all user-defined fields and filters system fields.
func TestGetAll(t *testing.T) {
	p := onepassword.NewWithRunner(
		onepassword.Config{ServiceAccountToken: "ops_token", Vault: "dev-chris"},
		makeRunner(func(args []string) ([]byte, error) {
			return sampleItemJSON, nil
		}),
	)

	got, err := p.GetAll(context.Background(), "k3d", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["GRAFANA_ADMIN_PASSWORD"] != "grafana-secret" {
		t.Errorf("GRAFANA_ADMIN_PASSWORD: want grafana-secret got %q", got["GRAFANA_ADMIN_PASSWORD"])
	}
	if got["DB_PASSWORD"] != "db-secret" {
		t.Errorf("DB_PASSWORD: want db-secret got %q", got["DB_PASSWORD"])
	}
	if _, ok := got["username"]; ok {
		t.Error("system field 'username' should be filtered out")
	}
}

// TestGetAll_ItemNotFound returns empty map when op exits non-zero.
func TestGetAll_ItemNotFound(t *testing.T) {
	p := onepassword.NewWithRunner(
		onepassword.Config{ServiceAccountToken: "ops_token", Vault: "dev-chris"},
		makeRunner(func(_ []string) ([]byte, error) {
			return nil, &fakeExitError{}
		}),
	)

	got, err := p.GetAll(context.Background(), "k3d", "")
	if err != nil {
		t.Fatalf("item-not-found should return empty map, not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

// TestGetAll_Args verifies the correct op arguments are passed.
func TestGetAll_Args(t *testing.T) {
	var gotArgs []string
	p := onepassword.NewWithRunner(
		onepassword.Config{ServiceAccountToken: "ops_token", Vault: "dev-chris"},
		makeRunner(func(args []string) ([]byte, error) {
			gotArgs = args
			return sampleItemJSON, nil
		}),
	)

	if _, err := p.GetAll(context.Background(), "hetzner", "ash"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expect: item get hetzner-ash --vault dev-chris --format json
	want := []string{"item", "get", "hetzner-ash", "--vault", "dev-chris", "--format", "json"}
	if len(gotArgs) != len(want) {
		t.Fatalf("args: want %v got %v", want, gotArgs)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Errorf("args[%d]: want %q got %q", i, want[i], gotArgs[i])
		}
	}
}

// TestGet returns a single field via op read with the correct op:// reference.
func TestGet(t *testing.T) {
	var capturedArgs []string
	p := onepassword.NewWithRunner(
		onepassword.Config{ServiceAccountToken: "ops_token", Vault: "dev-chris"},
		makeRunner(func(args []string) ([]byte, error) {
			capturedArgs = args
			return []byte("mypassword\n"), nil
		}),
	)

	val, err := p.Get(context.Background(), "hetzner", "ash", "MY_SECRET")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "mypassword" {
		t.Errorf("want mypassword got %q", val)
	}
	wantRef := "op://dev-chris/hetzner-ash/MY_SECRET"
	if len(capturedArgs) < 2 || capturedArgs[1] != wantRef {
		t.Errorf("op ref: want %q got args %v", wantRef, capturedArgs)
	}
}

// TestGet_k3d verifies empty region is omitted from the item title.
func TestGet_k3d(t *testing.T) {
	var capturedArgs []string
	p := onepassword.NewWithRunner(
		onepassword.Config{ServiceAccountToken: "ops_token", Vault: "dev-chris"},
		makeRunner(func(args []string) ([]byte, error) {
			capturedArgs = args
			return []byte("val\n"), nil
		}),
	)

	if _, err := p.Get(context.Background(), "k3d", "", "MY_SECRET"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantRef := "op://dev-chris/k3d/MY_SECRET"
	if len(capturedArgs) < 2 || capturedArgs[1] != wantRef {
		t.Errorf("op ref: want %q got args %v", wantRef, capturedArgs)
	}
}

// TestGet_ErrorDoesNotLeakPath verifies errors omit the op:// path.
func TestGet_ErrorDoesNotLeakPath(t *testing.T) {
	p := onepassword.NewWithRunner(
		onepassword.Config{ServiceAccountToken: "ops_token", Vault: "dev-chris"},
		makeRunner(func(_ []string) ([]byte, error) {
			return nil, &fakeExitError{}
		}),
	)

	_, err := p.Get(context.Background(), "k3d", "", "SECRET")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), "op://") {
		t.Errorf("error must not include op:// path, got: %v", err)
	}
}

// TestItemTitle covers the provider/region joining behaviour.
func TestItemTitle(t *testing.T) {
	cases := []struct {
		provider, region string
		want             string
	}{
		{"k3d", "", "k3d"},
		{"hetzner", "ash", "hetzner-ash"},
		{"", "", ""},
		{"hetzner", "", "hetzner"},
	}
	for _, tc := range cases {
		got := onepassword.ItemTitle(tc.provider, tc.region)
		if got != tc.want {
			t.Errorf("ItemTitle(%q,%q) = %q, want %q", tc.provider, tc.region, got, tc.want)
		}
	}
}

type fakeExitError struct{}

func (e *fakeExitError) Error() string { return "exit status 1" }
