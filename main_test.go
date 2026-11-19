package main

import (
	"strings"
	"testing"
)

const testCompose = `
x-common: &common
  restart: unless-stopped

services:
  web:
    <<: *common
    image: nginx:1.25
  db:
    image: postgres:16-alpine
  builder:
    build: .
  stats:
    image: ghcr.io/example/stats:latest
`

func serviceNames(svcs []composeService) []string {
	out := make([]string, len(svcs))
	for i, s := range svcs {
		out[i] = s.name
	}
	return out
}

func TestParseComposeServices(t *testing.T) {
	svcs, err := parseCompose([]byte(testCompose))
	if err != nil {
		t.Fatalf("parseCompose: %v", err)
	}
	got := strings.Join(serviceNames(svcs), ",")
	if want := "db,stats,web"; got != want {
		t.Fatalf("services = %q, want %q (build-only service must be skipped)", got, want)
	}
	if svcs[2].image != "nginx:1.25" {
		t.Fatalf("web image = %q", svcs[2].image)
	}
}

func TestParseComposeAnchorMerge(t *testing.T) {
	src := `
x-base: &base
  image: redis:7
  restart: unless-stopped
services:
  cache:
    <<: *base
`
	svcs, err := parseCompose([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 1 || svcs[0].image != "redis:7" {
		t.Fatalf("anchor merge image not resolved: %+v", svcs)
	}
}

func TestParseComposeErrors(t *testing.T) {
	if _, err := parseCompose([]byte("services: {}\n")); err == nil {
		t.Fatal("expected error for compose file without services")
	}
	if _, err := parseCompose([]byte("services:\n  app:\n    build: .\n")); err == nil {
		t.Fatal("expected error when no service has an image")
	}
}

func TestSelectComposeServices(t *testing.T) {
	svcs, err := parseCompose([]byte(testCompose))
	if err != nil {
		t.Fatal(err)
	}

	got, err := selectComposeServices(svcs, []string{"web", "db", "web"})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if names := strings.Join(serviceNames(got), ","); names != "web,db" {
		t.Fatalf("selection = %q, want requested order without duplicates", names)
	}

	if _, err := selectComposeServices(svcs, []string{"web", "nope"}); err == nil {
		t.Fatal("expected error for unknown service")
	} else if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error should name the unknown service: %v", err)
	}

	all, err := selectComposeServices(svcs, nil)
	if err != nil || len(all) != len(svcs) {
		t.Fatalf("empty selection should return all services: %v, %d", err, len(all))
	}
}

func TestEngineFromComposeBinary(t *testing.T) {
	cases := map[string]string{
		"docker":                        "docker",
		"docker-compose":                "docker",
		"/usr/bin/docker":               "docker",
		"podman":                        "podman",
		"podman-compose":                "podman",
		"/usr/local/bin/podman-compose": "podman",
		"unknown":                       "",
	}
	for bin, want := range cases {
		if got := engineFromComposeBinary(bin); got != want {
			t.Errorf("engineFromComposeBinary(%q) = %q, want %q", bin, got, want)
		}
	}
}

func TestResolveComposeCommandRejectsUnknownEngine(t *testing.T) {
	if _, _, err := resolveComposeCommand(Source{Engine: "lxc"}); err == nil {
		t.Fatal("expected error for unknown engine")
	}
}

func TestColorize(t *testing.T) {
	old := colorEnabled
	defer func() { colorEnabled = old }()

	colorEnabled = false
	if got := colorize(ansiGreen, "OK"); got != "OK" {
		t.Fatalf("colorize disabled = %q, want plain %q", got, "OK")
	}
	colorEnabled = true
	if got, want := colorize(ansiGreen, "OK"), "\x1b[32mOK\x1b[0m"; got != want {
		t.Fatalf("colorize enabled = %q, want %q", got, want)
	}
}
