package authz_test

import (
	"testing"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/authz"
)

func TestMatrix(t *testing.T) {
	for _, c := range []struct {
		role  authz.Role
		allow []authz.Action
		deny  []authz.Action
	}{
		{authz.NoRole, nil, []authz.Action{authz.Read, authz.Comment, authz.ManageTeam}},
		{authz.Viewer, []authz.Action{authz.Read}, []authz.Action{authz.CreateTask, authz.Comment, authz.Reply, authz.Approve, authz.Cancel, authz.ManageTeam}},
		{authz.Member, []authz.Action{authz.Read, authz.CreateTask, authz.Comment, authz.Reply, authz.Approve, authz.Cancel}, []authz.Action{authz.ManageTeam}},
		{authz.Admin, []authz.Action{authz.Read, authz.CreateTask, authz.Comment, authz.Reply, authz.Approve, authz.Cancel, authz.ManageTeam}, nil},
	} {
		for _, a := range c.allow {
			if !authz.Allowed(c.role, a) {
				t.Errorf("%s may not %s", c.role, a)
			}
		}
		for _, a := range c.deny {
			if authz.Allowed(c.role, a) {
				t.Errorf("%s may %s", c.role, a)
			}
		}
	}
	if authz.Allowed(authz.Admin, "rewrite history") {
		t.Error("an action outside the matrix is allowed")
	}
}

func team(roles ...v1alpha1.TeamRole) *v1alpha1.Team {
	return &v1alpha1.Team{Spec: v1alpha1.TeamSpec{Roles: roles}}
}

func role(name v1alpha1.RoleName, subjects ...v1alpha1.RoleSubject) v1alpha1.TeamRole {
	return v1alpha1.TeamRole{Role: name, Subjects: subjects}
}

func user(name string) v1alpha1.RoleSubject {
	return v1alpha1.RoleSubject{Kind: v1alpha1.SubjectUser, Name: name}
}

func group(name string) v1alpha1.RoleSubject {
	return v1alpha1.RoleSubject{Kind: v1alpha1.SubjectGroup, Name: name}
}

func TestRole(t *testing.T) {
	p := &authz.Policy{AdminUsers: []string{"root@example.test"}, AdminGroups: []string{"platform"}}
	dana := &auth.Identity{Username: "dana@example.test", Groups: []string{"backend", "oncall"}}
	tm := team(
		role(v1alpha1.RoleViewer, group("everyone"), group("backend")),
		role(v1alpha1.RoleMember, user("dana@example.test")),
		role(v1alpha1.RoleAdmin, group("leads")),
		role("owner", user("dana@example.test")), // unknown: gives nothing
	)
	for name, c := range map[string]struct {
		id   *auth.Identity
		team *v1alpha1.Team
		want authz.Role
	}{
		"strongest of user and group": {dana, tm, authz.Member},
		"by group only":               {&auth.Identity{Username: "eve@example.test", Groups: []string{"backend"}}, tm, authz.Viewer},
		"admin by group":              {&auth.Identity{Username: "lee@example.test", Groups: []string{"leads", "everyone"}}, tm, authz.Admin},
		"no match":                    {&auth.Identity{Username: "mallory@example.test", Groups: []string{"dana@example.test"}}, tm, authz.NoRole},
		"a group named like a user":   {&auth.Identity{Username: "x", Groups: []string{"dana@example.test"}}, team(role(v1alpha1.RoleMember, user("dana@example.test"))), authz.NoRole},
		"bootstrap user, no roles":    {&auth.Identity{Username: "root@example.test"}, team(), authz.Admin},
		"bootstrap group, no team":    {&auth.Identity{Username: "p", Groups: []string{"platform"}}, nil, authz.Admin},
		"no roles":                    {dana, team(), authz.NoRole},
		"no identity":                 {nil, tm, authz.NoRole},
	} {
		got, ok := p.Role(c.id, c.team)
		if got != c.want || ok != (c.want != authz.NoRole) {
			t.Errorf("%s: %s, %v; want %s", name, got, ok, c.want)
		}
	}
}

func TestRefusal(t *testing.T) {
	if got := authz.Refusal("team-a", authz.Viewer, authz.Comment); got != "to comment on team-a needs the member role; you are a viewer" {
		t.Errorf("%q", got)
	}
	if got := authz.Refusal("team-a", authz.NoRole, authz.Read); got != "to read on team-a needs the viewer role; you have no role there" {
		t.Errorf("%q", got)
	}
}

// Usernames fold as the login lower-cased them (strings.ToLower), and only
// so: EqualFold would also take a long s for an s, which ToLower keeps
// apart. The Kelvin sign lower-cases to k, on both sides alike.
func TestFold(t *testing.T) {
	p := &authz.Policy{AdminUsers: []string{"Root@Example.test", "ſam@example.test"}, FoldUsernames: true}
	tm := team(role(v1alpha1.RoleMember, user("Dana@Example.test")), role(v1alpha1.RoleViewer, user("Kim@example.test")))
	for username, want := range map[string]authz.Role{
		"dana@example.test": authz.Member, // usernames come lower-cased
		"root@example.test": authz.Admin,
		"sam@example.test":  authz.NoRole,
		"kim@example.test":  authz.Viewer,
	} {
		if got, _ := p.Role(&auth.Identity{Username: username}, tm); got != want {
			t.Errorf("%s: %s, want %s", username, got, want)
		}
	}
}

// An agent has its AgentAccess's role, at most member, on its own
// namespace's team only: no team role naming it, nor a bootstrap admin
// entry, gives it more, and it has none anywhere else.
func TestAgentRole(t *testing.T) {
	bot := auth.AgentIdentity("team-a", "bot", "member")
	p := &authz.Policy{AdminUsers: []string{bot.Username}, AdminGroups: []string{"platform"}}
	own := team(role(v1alpha1.RoleAdmin, user(bot.Username)))
	own.Namespace = "team-a"
	other := team(role(v1alpha1.RoleAdmin, user(bot.Username)))
	other.Namespace = "team-b"
	for name, c := range map[string]struct {
		id   *auth.Identity
		team *v1alpha1.Team
		want authz.Role
	}{
		"its own team, despite an admin entry": {bot, own, authz.Member},
		"a viewer":                             {auth.AgentIdentity("team-a", "ro", "viewer"), own, authz.Viewer},
		"an admin role, were one written":      {auth.AgentIdentity("team-a", "x", "admin"), own, authz.Member},
		"an unknown role":                      {auth.AgentIdentity("team-a", "x", "owner"), own, authz.NoRole},
		"another team":                         {bot, other, authz.NoRole},
		"no team":                              {bot, nil, authz.NoRole},
		"groups, which an agent has none of":   {&auth.Identity{Username: bot.Username, Groups: []string{"platform"}, Agent: bot.Agent}, other, authz.NoRole},
	} {
		got, ok := p.Role(c.id, c.team)
		if got != c.want || ok != (c.want != authz.NoRole) {
			t.Errorf("%s: %s, %v; want %s", name, got, ok, c.want)
		}
	}
	if p.Bootstrap(bot) {
		t.Error("an agent named in --admin-users is a bootstrap admin")
	}
}
