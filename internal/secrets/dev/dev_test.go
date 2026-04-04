package dev_test

import (
	"context"
	"errors"
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
