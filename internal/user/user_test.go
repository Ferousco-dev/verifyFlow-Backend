package user

import (
	"regexp"
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Feranmi Oresajo":                 "feranmi-oresajo",
		"  Ada   Lovelace  ":              "ada-lovelace",
		"Jean-Luc O'Brien":                "jean-luc-o-brien",
		"李雷":                              "user",
		"":                                "user",
		"!!!":                             "user",
		"José Álvarez":                    "jos-lvarez",
		strings.Repeat("a", 80):           strings.Repeat("a", 30),
		"John " + strings.Repeat("b", 40): "john-bbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	valid := regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	for in, want := range cases {
		got := Slugify(in)
		if got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
		if !valid.MatchString(got) {
			t.Errorf("Slugify(%q) = %q violates username format", in, got)
		}
	}
}
