package server

import (
	"net/http"
	"testing"
)

func TestValidateHostCorrect(t *testing.T) {
	r := &http.Request{Host: "127.0.0.1:49821"}
	if !ValidateHost(r, 49821) {
		t.Error("expected true for 127.0.0.1:49821")
	}
}

func TestValidateHostLocalhost(t *testing.T) {
	r := &http.Request{Host: "localhost:49821"}
	if !ValidateHost(r, 49821) {
		t.Error("expected true for localhost:49821")
	}
}

func TestValidateHostWrongPort(t *testing.T) {
	r := &http.Request{Host: "127.0.0.1:9999"}
	if ValidateHost(r, 49821) {
		t.Error("expected false for wrong port")
	}
}

func TestValidateHostEvil(t *testing.T) {
	r := &http.Request{Host: "evil.com:49821"}
	if ValidateHost(r, 49821) {
		t.Error("expected false for evil.com")
	}
}

func TestValidateHostNoPort(t *testing.T) {
	r := &http.Request{Host: "127.0.0.1"}
	if ValidateHost(r, 49821) {
		t.Error("expected false for missing port")
	}
}

func TestValidateHostEmpty(t *testing.T) {
	r := &http.Request{Host: ""}
	if ValidateHost(r, 49821) {
		t.Error("expected false for empty host")
	}
}

func TestValidatePathSuccess(t *testing.T) {
	p, err := ValidatePath("Documents/file.htmlclay", "/Users/david")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != "/Users/david/Documents/file.htmlclay" {
		t.Errorf("got %q", p)
	}
}

func TestValidatePathSimple(t *testing.T) {
	p, err := ValidatePath("file.htmlclay", "/Users/david")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != "/Users/david/file.htmlclay" {
		t.Errorf("got %q", p)
	}
}

func TestValidatePathNested(t *testing.T) {
	p, err := ValidatePath("a/b/c/file.htmlclay", "/Users/david")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != "/Users/david/a/b/c/file.htmlclay" {
		t.Errorf("got %q", p)
	}
}

func TestValidatePathTraversal(t *testing.T) {
	_, err := ValidatePath("../../../etc/passwd", "/Users/david")
	if err == nil {
		t.Error("expected error for path traversal")
	}
}

func TestValidatePathTraversalMiddle(t *testing.T) {
	_, err := ValidatePath("Documents/../../etc/passwd", "/Users/david")
	if err == nil {
		t.Error("expected error for mid-path traversal")
	}
}

func TestValidatePathAbsolute(t *testing.T) {
	_, err := ValidatePath("/etc/passwd", "/Users/david")
	if err == nil {
		t.Error("expected error for absolute path")
	}
}

func TestValidatePathNullByte(t *testing.T) {
	_, err := ValidatePath("Documents/file\x00.htmlclay", "/Users/david")
	if err == nil {
		t.Error("expected error for null byte")
	}
}

func TestValidatePathNormalized(t *testing.T) {
	p, err := ValidatePath("Documents/../Documents/file.htmlclay", "/Users/david")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != "/Users/david/Documents/file.htmlclay" {
		t.Errorf("got %q", p)
	}
}
