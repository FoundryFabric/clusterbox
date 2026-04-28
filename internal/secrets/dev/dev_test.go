package dev_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/secrets/dev"
)

const sampleJSON = `{"JWT_SECRET":"dev-jwt","DB_PASSWORD":"dev-pass"}`

func TestGetAll_ReadsAllKeys(t *testing.T) {
	p := dev.NewWithReader("", func(_ string) ([]byte, error) {
		return []byte(sampleJSON), nil
	})

	got, err := p.GetAll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]string{
		"JWT_SECRET":  "dev-jwt",
		"DB_PASSWORD": "dev-pass",
	}
	for k, wv := range want {
		if gv, ok := got[k]; !ok {
			t.Errorf("key %q missing", k)
		} else if gv != wv {
			t.Errorf("key %q: want %q got %q", k, wv, gv)
		}
	}
}

func TestGet_ReturnsSingleKey(t *testing.T) {
	p := dev.NewWithReader("", func(_ string) ([]byte, error) {
		return []byte(sampleJSON), nil
	})

	val, err := p.Get(context.Background(), "JWT_SECRET")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "dev-jwt" {
		t.Errorf("want %q got %q", "dev-jwt", val)
	}
}

func TestGet_MissingKeyReturnsError(t *testing.T) {
	p := dev.NewWithReader("", func(_ string) ([]byte, error) {
		return []byte(sampleJSON), nil
	})

	_, err := p.Get(context.Background(), "NONEXISTENT")
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
	if !strings.Contains(err.Error(), "NONEXISTENT") {
		t.Errorf("error should mention the missing key, got: %v", err)
	}
}

func TestGetAll_MissingFileReturnsHelpfulError(t *testing.T) {
	p := dev.NewWithReader("deploy/config/dev.secrets.json", func(_ string) ([]byte, error) {
		return nil, errors.New("no such file or directory")
	})

	_, err := p.GetAll(context.Background())
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "dev.secrets.example.json") {
		t.Errorf("error should mention dev.secrets.example.json, got: %v", err)
	}
}

func TestGetAll_InvalidJSONReturnsError(t *testing.T) {
	p := dev.NewWithReader("", func(_ string) ([]byte, error) {
		return []byte(`not json`), nil
	})

	_, err := p.GetAll(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestGetAll_OPRefResolved(t *testing.T) {
	p := dev.NewWithReader("", func(_ string) ([]byte, error) {
		return []byte(`{"PLAIN":"value","SECRET":"op://vault/item/field"}`), nil
	})
	p.RunOpFn = func(ref string) (string, error) {
		if ref == "op://vault/item/field" {
			return "resolved-secret", nil
		}
		return "", fmt.Errorf("unexpected ref %q", ref)
	}

	got, err := p.GetAll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["PLAIN"] != "value" {
		t.Errorf("PLAIN: want %q got %q", "value", got["PLAIN"])
	}
	if got["SECRET"] != "resolved-secret" {
		t.Errorf("SECRET: want %q got %q", "resolved-secret", got["SECRET"])
	}
}

func TestGetAll_OPRefError(t *testing.T) {
	p := dev.NewWithReader("", func(_ string) ([]byte, error) {
		return []byte(`{"SECRET":"op://vault/item/field"}`), nil
	})
	p.RunOpFn = func(_ string) (string, error) {
		return "", errors.New("not signed in")
	}

	_, err := p.GetAll(context.Background())
	if err == nil {
		t.Fatal("expected error from op:// resolution, got nil")
	}
	if !strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error should mention key name, got: %v", err)
	}
}
