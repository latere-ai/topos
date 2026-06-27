package sdk

import "strings"

// Directory returns the discoverable peers of a region as cards — name, role, and
// when-to-use description, never permissions. This is the workspace-wide view a
// dynamic agent sees; whom it may actually message stays capability-gated by the
// delegate tool (a peer not in this set is refused).
func (r Region) Directory() []PeerCard {
	cards := make([]PeerCard, 0, len(r.Peers))
	for _, p := range r.Peers {
		cards = append(cards, PeerCard{Name: p.Name, Role: p.Role, Description: p.Description})
	}
	return cards
}

// renderDirectory formats a region's directory for injection into a dynamic
// agent's system prompt, so the model chooses peers from their descriptions
// rather than guessing from names alone.
func renderDirectory(cards []PeerCard) string {
	if len(cards) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("You can delegate a subtask to a peer agent with the `delegate` tool. Available peers:\n")
	for _, c := range cards {
		line := "- " + c.Name
		if c.Role != "" {
			line += " (" + c.Role + ")"
		}
		if c.Description != "" {
			line += ": " + c.Description
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("Pick the peer whose description best fits the subtask; if none fit, do the work yourself.")
	return b.String()
}

// composeSystem joins an agent's own system prompt with the injected directory.
func composeSystem(base, dir string) string {
	switch {
	case base == "":
		return dir
	case dir == "":
		return base
	default:
		return base + "\n\n" + dir
	}
}
