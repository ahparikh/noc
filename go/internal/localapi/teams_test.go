package localapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"noc.local/noc/internal/registry"
	apiv1 "noc.local/noc/pkg/api/v1"
)

func teamHandler(t *testing.T, ids ...string) (*registry.Memory, string, *Client) {
	t.Helper()
	mem := registry.NewMemory()
	for _, id := range ids {
		if err := mem.Upsert(context.Background(), newAgent(id)); err != nil {
			t.Fatal(err)
		}
	}
	handler := NewHandler(mem, testHealth)
	handler.SetSpawnSupport("host-1", nil)
	socket, client := serve(t, handler)
	return mem, socket, client
}

func TestTeamMembershipAndLead(t *testing.T) {
	ctx := context.Background()
	mem, _, client := teamHandler(t, "a1", "a2", "a3")

	for _, id := range []string{"a1", "a2"} {
		agent, err := client.SetAgentTeam(ctx, id, "auth")
		if err != nil || agent.Team != "auth" || agent.Lead {
			t.Fatalf("set %s: %+v %v", id, agent, err)
		}
	}
	// A lead must already be in the team it is to lead.
	if _, err := client.SetTeamLead(ctx, "a3", "auth"); err == nil || err.Error() != "noc server: a3 is not in team auth" {
		t.Fatalf("lead outside the team: %v", err)
	}
	if agent, err := client.SetTeamLead(ctx, "a1", "auth"); err != nil || !agent.Lead {
		t.Fatalf("lead a1: %+v %v", agent, err)
	}
	// Marking another clears the first.
	if agent, err := client.SetTeamLead(ctx, "a2", "auth"); err != nil || !agent.Lead {
		t.Fatalf("lead a2: %+v %v", agent, err)
	}
	if a1, _ := mem.Get(ctx, "a1"); a1.Lead || a1.Team != "auth" {
		t.Fatalf("a1 should be an ordinary member: %+v", a1)
	}
	// Setting the same team again keeps the lead; moving teams drops it.
	if agent, _ := client.SetAgentTeam(ctx, "a2", "auth"); !agent.Lead {
		t.Fatalf("same team must keep the lead: %+v", agent)
	}
	if agent, _ := client.SetAgentTeam(ctx, "a2", "billing"); agent.Lead || agent.Team != "billing" {
		t.Fatalf("moving must drop the lead: %+v", agent)
	}
	if agent, err := client.SetAgentTeam(ctx, "a2", ""); err != nil || agent.Team != "" || agent.Lead {
		t.Fatalf("clear: %+v %v", agent, err)
	}
	// Reconciliation and hooks upsert whole agents; the team survives them.
	if err := mem.Upsert(ctx, newAgent("a1")); err != nil {
		t.Fatal(err)
	}
	if a1, _ := mem.Get(ctx, "a1"); a1.Team != "auth" {
		t.Fatalf("upsert lost the team: %+v", a1)
	}
}

func TestTeamAndSpawnErrorCatalog(t *testing.T) {
	_, socket, _ := teamHandler(t, "a1")
	cases := []struct {
		method, path, body string
		status             int
		code, msg          string
	}{
		{"GET", "/v1/agents/a1/team", "", 405, "method_not_allowed", ""},
		{"PUT", "/v1/agents/a1/team", ``, 400, "invalid_team", "request body must contain a team"},
		{"PUT", "/v1/agents/a1/team", `{"team":"x","extra":1}`, 400, "invalid_team", ""},
		{"PUT", "/v1/agents/a1/team", `{"team":"none"}`, 400, "invalid_team", invalidTeamMessage},
		{"PUT", "/v1/agents/a1/team", `{"team":"-x"}`, 400, "invalid_team", ""},
		{"PUT", "/v1/agents/a1/team", `{"team":"","lead":true}`, 400, "invalid_team", "a lead needs a team"},
		{"PUT", "/v1/agents/a1/team", `{"team":"auth","lead":true}`, 409, "not_in_team", "a1 is not in team auth"},
		{"PUT", "/v1/agents/missing/team", `{"team":"auth"}`, 404, "agent_not_found", ""},
		{"PUT", "/v1/agents/missing/team", `{"team":"auth","lead":true}`, 404, "agent_not_found", ""},
		{"DELETE", "/v1/agents", "", 405, "method_not_allowed", ""},
		{"POST", "/v1/agents", ``, 400, "invalid_spawn", "request body must describe a spawned agent"},
		{"POST", "/v1/agents", `{"provider":"other","native_session_id":"s"}`, 400, "invalid_provider", ""},
		{"POST", "/v1/agents", `{"provider":"claude"}`, 400, "invalid_spawn", "a native session ID is required"},
		{"POST", "/v1/agents", `{"provider":"claude","native_session_id":"s","team":"a b"}`, 400, "invalid_team", ""},
		{"POST", "/v1/agents", `{"provider":"claude","native_session_id":"s","parent_id":"gone"}`, 400, "invalid_parent", "the parent agent was not found"},
		{"POST", "/v1/agents", `{"provider":"claude","native_session_id":"s","name":"` + strings.Repeat("x", 121) + `"}`, 400, "invalid_agent_name", ""},
	}
	for _, c := range cases {
		resp, body := rawDo(t, socket, c.method, c.path, c.body)
		var e apiv1.Error
		if err := json.Unmarshal([]byte(body), &e); err != nil || resp.StatusCode != c.status || e.Code != c.code || (c.msg != "" && e.Message != c.msg) {
			t.Errorf("%s %s %s: status=%d body=%s", c.method, c.path, c.body, resp.StatusCode, body)
		}
	}
}

func TestSpawnRegistrationRules(t *testing.T) {
	ctx := context.Background()
	mem, _, client := teamHandler(t, "boss", "member", "loner")
	spawn := func(session, team, parent string) apiv1.Agent {
		t.Helper()
		agent, err := client.RegisterSpawn(ctx, apiv1.AgentSpawn{
			Provider: apiv1.ProviderClaude, NativeSessionID: session, Team: team, ParentID: parent,
			Name: "worker " + session, Directory: "/work/app", Prompt: "fix the tests",
		})
		if err != nil {
			t.Fatalf("spawn %s: %v", session, err)
		}
		return agent
	}
	get := func(id string) apiv1.Agent {
		t.Helper()
		agent, err := mem.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return agent
	}

	// From outside any agent: the named team, no parent, nobody made lead.
	w0 := spawn("s0", "auth", "")
	if w0.Team != "auth" || w0.Lead || w0.ParentID != "" || w0.Origin != "spawn" ||
		w0.RuntimeState != apiv1.RuntimeStateStarting || w0.LiveState != apiv1.LiveStateLive ||
		w0.Name != "worker s0" || w0.HostID != "host-1" || w0.Provider != apiv1.ProviderClaude ||
		w0.InitialRequest == nil || w0.InitialRequest.Text != "fix the tests" || w0.InitialRequest.Source != "spawn" ||
		w0.Workspace == nil || w0.Workspace.Path != "/work/app" {
		t.Fatalf("spawned agent: %+v", w0)
	}

	// An agent in no team that names a team with no lead joins it and leads it.
	w1 := spawn("s1", "auth", "boss")
	if w1.Team != "auth" || w1.Lead || w1.ParentID != "boss" {
		t.Fatalf("first worker: %+v", w1)
	}
	if boss := get("boss"); boss.Team != "auth" || !boss.Lead {
		t.Fatalf("the spawner should lead: %+v", boss)
	}

	// No team named: the caller's team is inherited, and "none" opts out.
	if w2 := spawn("s2", "", "boss"); w2.Team != "auth" || w2.ParentID != "boss" {
		t.Fatalf("inherited: %+v", w2)
	}
	if w3 := spawn("s3", "none", "boss"); w3.Team != "" || w3.ParentID != "boss" {
		t.Fatalf("opted out: %+v", w3)
	}
	if w4 := spawn("s4", "", "loner"); w4.Team != "" || w4.ParentID != "loner" {
		t.Fatalf("nothing to inherit: %+v", w4)
	}

	// A team that has a lead keeps it, and the spawner stays where it was.
	spawn("s5", "auth", "loner")
	if loner := get("loner"); loner.Team != "" || loner.Lead {
		t.Fatalf("a led team must not take the spawner: %+v", loner)
	}
	if boss := get("boss"); !boss.Lead {
		t.Fatalf("lead changed: %+v", boss)
	}

	// A member of another team that spawns into a team is not moved.
	if _, err := client.SetAgentTeam(ctx, "member", "billing"); err != nil {
		t.Fatal(err)
	}
	spawn("s6", "docs", "member")
	if member := get("member"); member.Team != "billing" || member.Lead {
		t.Fatalf("a member of another team must stay: %+v", member)
	}

	// When a hook created the row first, the registration lands on that row.
	early := newAgent("early")
	early.Provider, early.NativeSessionID = apiv1.ProviderClaude, "s7"
	if err := mem.Upsert(ctx, early); err != nil {
		t.Fatal(err)
	}
	if adopted := spawn("s7", "auth", "boss"); adopted.ID != "early" || adopted.Team != "auth" || adopted.ParentID != "boss" {
		t.Fatalf("adopted: %+v", adopted)
	}

	// A long prompt is cut and marked.
	long, err := client.RegisterSpawn(ctx, apiv1.AgentSpawn{
		Provider: apiv1.ProviderClaude, NativeSessionID: "s8", Prompt: strings.Repeat("é", 3000),
	})
	if err != nil || len([]rune(long.InitialRequest.Text)) != 2048 || !long.InitialRequest.Truncated || long.Workspace != nil {
		t.Fatalf("long prompt: %+v %v", long.InitialRequest, err)
	}
}

func TestSpawnIsUnavailableWithoutAHost(t *testing.T) {
	mem := registry.NewMemory()
	socket, _ := serve(t, NewHandler(mem, testHealth))
	resp, body := rawDo(t, socket, "POST", "/v1/agents", `{"provider":"claude","native_session_id":"s"}`)
	if resp.StatusCode != 501 || !strings.Contains(body, "agent_spawn_unavailable") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}
