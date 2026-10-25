// Package authz decides what a logged-in person may do on a team's board:
// their role on the team, from Team.spec.roles or as a bootstrap admin, and
// a fixed matrix of what each role allows. An agent outside Cadre has its
// AgentAccess's role, on its own namespace's team alone.
package authz

import (
	"fmt"
	"slices"
	"strings"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/auth"
)

// Role is a role on a team, ordered: each allows what the ones below it do.
type Role int

// Roles, weakest first.
const (
	NoRole Role = iota
	Viewer
	Member
	Admin
)

func (r Role) String() string {
	switch r {
	case Viewer:
		return string(v1alpha1.RoleViewer)
	case Member:
		return string(v1alpha1.RoleMember)
	case Admin:
		return string(v1alpha1.RoleAdmin)
	}
	return "none"
}

func parse(name v1alpha1.RoleName) Role {
	switch name {
	case v1alpha1.RoleViewer:
		return Viewer
	case v1alpha1.RoleMember:
		return Member
	case v1alpha1.RoleAdmin:
		return Admin
	}
	return NoRole // an unknown role gives nothing
}

// Action is something done on a team's board.
type Action string

// Actions.
const (
	// Read is listing and reading tasks, threads, runs, events and
	// agents, and the Team itself.
	Read Action = "read"
	// The work on tasks.
	CreateTask Action = "create tasks"
	Comment    Action = "comment"
	Reply      Action = "reply"
	Approve    Action = "approve or deny tool calls"
	Cancel     Action = "cancel tasks"
	// ManageTeam is initialising or editing the Team.
	ManageTeam Action = "manage the team"
)

// needs is the matrix: the weakest role that may take each action.
var needs = map[Action]Role{
	Read:       Viewer,
	CreateTask: Member,
	Comment:    Member,
	Reply:      Member,
	Approve:    Member,
	Cancel:     Member,
	ManageTeam: Admin,
}

// Needs is the weakest role allowed a; an action not in the matrix needs
// more than any role.
func Needs(a Action) Role {
	if r, ok := needs[a]; ok {
		return r
	}
	return Admin + 1
}

// Allowed reports whether role may take a.
func Allowed(role Role, a Action) bool { return role >= Needs(a) }

// Policy is how roles are found. Without logins (--auth=none) there is no
// Policy: everyone may do anything.
type Policy struct {
	// AdminUsers and AdminGroups are bootstrap admins: admin on every
	// team, and allowed to create teams.
	AdminUsers, AdminGroups []string
	// FoldUsernames compares usernames without case, as email addresses
	// are: identities then carry them lower-cased with strings.ToLower,
	// and names from roles and flags are lower-cased the same way.
	FoldUsernames bool
}

// sameUser compares a username from a role or a flag with an identity's.
func (p *Policy) sameUser(name, username string) bool {
	if p.FoldUsernames {
		// The same fold as the identity's username had, not EqualFold,
		// which also takes a long s for an s.
		return strings.ToLower(name) == username
	}
	return name == username
}

// Bootstrap reports whether id is a bootstrap admin: an agent never is.
func (p *Policy) Bootstrap(id *auth.Identity) bool {
	if id == nil || id.Agent != nil {
		return false
	}
	if slices.ContainsFunc(p.AdminUsers, func(u string) bool { return p.sameUser(u, id.Username) }) {
		return true
	}
	for _, g := range id.Groups {
		if slices.Contains(p.AdminGroups, g) {
			return true
		}
	}
	return false
}

// Role is id's role on team, the strongest of the entries that name it or
// one of its groups; ok says it has one. An agent's is its AgentAccess's,
// at most member, on the team of the AgentAccess's namespace and no other:
// no entry of the team's roles, nor a bootstrap admin, applies to it.
func (p *Policy) Role(id *auth.Identity, team *v1alpha1.Team) (Role, bool) {
	if id != nil && id.Agent != nil {
		if team == nil || team.Namespace != id.Agent.Namespace {
			return NoRole, false
		}
		role := min(parse(v1alpha1.RoleName(id.Agent.Role)), Member)
		return role, role > NoRole
	}
	if p.Bootstrap(id) {
		return Admin, true
	}
	if id == nil || team == nil {
		return NoRole, false
	}
	best := NoRole
	for _, r := range team.Spec.Roles {
		if role := parse(r.Role); role > best && p.matches(id, r.Subjects) {
			best = role
		}
	}
	return best, best > NoRole
}

func (p *Policy) matches(id *auth.Identity, subjects []v1alpha1.RoleSubject) bool {
	for _, s := range subjects {
		switch s.Kind {
		case v1alpha1.SubjectUser:
			if s.Name != "" && p.sameUser(s.Name, id.Username) {
				return true
			}
		case v1alpha1.SubjectGroup:
			if s.Name != "" && slices.Contains(id.Groups, s.Name) {
				return true
			}
		}
	}
	return false
}

// Refusal says why role may not take a on the team in namespace ns.
func Refusal(ns string, role Role, a Action) string {
	if role == NoRole {
		return fmt.Sprintf("to %s on %s needs the %s role; you have no role there", a, ns, Needs(a))
	}
	return fmt.Sprintf("to %s on %s needs the %s role; you are a %s", a, ns, Needs(a), role)
}
