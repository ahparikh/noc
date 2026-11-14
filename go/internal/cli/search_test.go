package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	apiv1 "noc.local/noc/pkg/api/v1"
)

func TestParseAge(t *testing.T) {
	day := 24 * time.Hour
	for value, want := range map[string]time.Duration{
		"14": 14 * day, "14d": 14 * day, " 2W ": 14 * day, "36h": 36 * time.Hour, "1": day,
	} {
		got, err := parseAge(value)
		if err != nil || got != want {
			t.Errorf("parseAge(%q) = %v, %v; want %v", value, got, err, want)
		}
	}
	for _, bad := range []string{"", "d", "0", "-3", "1.5", "two", "3m", "999999999"} {
		if _, err := parseAge(bad); err == nil {
			t.Errorf("parseAge(%q) succeeded", bad)
		}
	}
	if _, err := parseAge("soon"); err == nil ||
		err.Error() != `invalid age "soon" (want a number of days such as 14, or 14d, 2w, 36h)` {
		t.Fatalf("err = %v", err)
	}
}

func TestParseSearchQuery(t *testing.T) {
	day := 24 * time.Hour
	query, err := parseSearchQuery([]string{"Auth  migration", "older:2w", "NEWER:60"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(query.terms, ",") != "auth,migration" || query.olderThan != 14*day || query.newerThan != 60*day {
		t.Fatalf("query = %+v", query)
	}
	// A word in the query overrides the flag; a flag alone is enough.
	query, err = parseSearchQuery([]string{"older:3"}, "30", "90d")
	if err != nil || query.olderThan != 3*day || query.newerThan != 90*day || len(query.terms) != 0 {
		t.Fatalf("query = %+v, %v", query, err)
	}
	// Half-typed bounds are text, so a line being typed never fails.
	query, err = parseSearchQuery([]string{"older:", "newer:x", "older"}, "", "")
	if err != nil || strings.Join(query.terms, ",") != "older:,newer:x,older" || query.olderThan != 0 {
		t.Fatalf("query = %+v, %v", query, err)
	}
	if _, err := parseSearchQuery(nil, "x", ""); err == nil {
		t.Fatal("a bad --older-than was accepted")
	}
	if _, err := parseSearchQuery(nil, "", "0"); err == nil {
		t.Fatal("a bad --newer-than was accepted")
	}
	if _, err := parseSearchQuery([]string{"older:30"}, "", "7"); err == nil ||
		err.Error() != "no agent is both older than 4w and newer than 7d" {
		t.Fatalf("err = %v", err)
	}
}

func TestSearchAgentsByTextAndAge(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	ago := func(days int) *time.Time { v := now.Add(-time.Duration(days) * 24 * time.Hour); return &v }
	agents := []apiv1.Agent{
		{ID: "a-fresh", Name: "Auth migration", Provider: apiv1.ProviderClaude, Bucket: apiv1.BucketWorking,
			LastActivityAt: ago(0), Workspace: &apiv1.Workspace{Path: "/code/app", Branch: "main"}},
		{ID: "b-week", UserName: "Cache refactor", Provider: apiv1.ProviderCodex, Bucket: apiv1.BucketQuiet,
			LastActivityAt: ago(8), InitialRequest: &apiv1.TextPreview{Text: "Rework the AUTH cache"}},
		{ID: "c-month", Name: "Docs pass", Provider: apiv1.ProviderClaude, Bucket: apiv1.BucketQuiet,
			EndedAt: ago(40), Workspace: &apiv1.Workspace{Path: "/code/docs", Branch: "auth-notes"}},
		// LastSeenAt is when noc last looked, not when the agent was used.
		{ID: "d-seen", Name: "Stale", Bucket: apiv1.BucketQuiet, FirstSeenAt: *ago(100), LastSeenAt: now},
	}
	ids := func(words []string, older, newer string) string {
		t.Helper()
		query, err := parseSearchQuery(words, older, newer)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, agent := range searchAgents(agents, query, now) {
			out = append(out, agent.ID)
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		words        []string
		older, newer string
		want         string
	}{
		{nil, "", "", "a-fresh,b-week,c-month,d-seen"},
		{[]string{"auth"}, "", "", "a-fresh,b-week,c-month"}, // name, request, branch
		{[]string{"auth", "claude"}, "", "", "a-fresh,c-month"},
		{[]string{"auth codex"}, "", "", "b-week"},
		{[]string{"/code/docs"}, "", "", "c-month"},
		{[]string{"nothing-like-this"}, "", "", ""},
		{nil, "7", "", "b-week,c-month,d-seen"},
		{nil, "8", "", "b-week,c-month,d-seen"}, // exactly 8 days is old enough
		{nil, "", "8", "a-fresh,b-week"},
		{nil, "7", "60", "b-week,c-month"},
		{[]string{"older:2w", "quiet"}, "", "", "c-month,d-seen"},
		{[]string{"auth", "older:30"}, "", "", "c-month"},
	} {
		if got := ids(c.words, c.older, c.newer); got != c.want {
			t.Errorf("search %q older=%q newer=%q = %q, want %q", c.words, c.older, c.newer, got, c.want)
		}
	}

	var out bytes.Buffer
	renderSearchResults(&out, agents[1:3], now)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || strings.Join(strings.Fields(lines[0]), " ") != "ID PROVIDER BUCKET STATE AGE WORKSPACE NAME" {
		t.Fatalf("table = %q", out.String())
	}
	if f := strings.Fields(lines[1]); f[0] != "b-week" || f[3] != "8d" {
		t.Fatalf("row = %q", lines[1])
	}
	if f := strings.Fields(lines[2]); f[0] != "c-month" || f[3] != "5w" {
		t.Fatalf("row = %q", lines[2])
	}
	out.Reset()
	renderSearchResults(&out, nil, now)
	if out.String() != "No agents match.\n" {
		t.Fatalf("empty = %q", out.String())
	}
	for age, want := range map[time.Duration]string{
		-time.Minute: "0m", 40 * time.Minute: "40m", 5 * time.Hour: "5h",
		13 * 24 * time.Hour: "13d", 14 * 24 * time.Hour: "2w",
	} {
		if got := formatAge(age); got != want {
			t.Errorf("formatAge(%v) = %q, want %q", age, got, want)
		}
	}
}

func TestSearchRejectsBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{"search", "--older-than", "soon"},
		{"search", "--bucket", "bogus"},
		{"search", "--older-than", "30", "--newer-than", "7"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(t.Context(), args, &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit %d, stderr %q", args, code, stderr.String())
		}
		if !strings.HasPrefix(stderr.String(), "noc search: ") {
			t.Errorf("%v: stderr %q", args, stderr.String())
		}
	}
}
