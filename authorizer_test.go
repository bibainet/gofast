package gofast_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bibainet/gofast"
)

func TestNewAuthRequest(t *testing.T) {
	// generate new request to clone from
	content := "hello world 1234"
	raw, err := http.NewRequest(
		"POST",
		"http://foobar.com/hello",
		strings.NewReader(content),
	)
	if err != nil {
		t.Errorf("unexpected error: %s", err)
		return
	}
	// clone the request into 2
	r, req, err := gofast.NewAuthRequest(raw)
	if err != nil {
		t.Errorf("unexpected error: %s", err)
		return
	}

	// the original body is read and became "empty"
	body, err := io.ReadAll(raw.Body)
	if err != nil {
		t.Errorf("unexpected error: %s", err)
		return
	}
	if want, have := "", string(body); want != have {
		t.Errorf("expected: %#v, got %#v", want, have)
	}

	// examine the r.Body reader
	body, err = io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("unexpected error: %s", err)
		return
	}
	if want, have := content, string(body); want != have {
		t.Errorf("expected: %#v, got %#v", want, have)
	}

	// examine the req.Stdin reader
	stdin, err := io.ReadAll(req.Stdin)
	if err != nil {
		t.Errorf("unexpected error: %s", err)
		return
	}
	if want, have := content, string(stdin); want != have {
		t.Errorf("expected: %#v, got %#v", want, have)
	}
}
