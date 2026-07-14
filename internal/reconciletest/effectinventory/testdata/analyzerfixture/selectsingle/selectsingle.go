// Package selectsingle exercises blocking one-case select classification.
package selectsingle

// Target is the value carried by the fixture channel.
type Target string

// Hub owns the exact inventoried wake channel.
type Hub struct {
	Wake chan Target
}

// BlockingSelectSend sends through a blocking one-case select. SSA may lower
// this source form to a plain send, but discovery must preserve select intent.
func BlockingSelectSend(hub *Hub, target Target) {
	select { //nolint:staticcheck // This fixture proves one-case select source identity.
	case hub.Wake <- target:
	}
}

// BlockingSelectReceive receives through a blocking one-case select. SSA may
// lower this source form to a plain receive, but discovery must preserve it.
func BlockingSelectReceive(hub *Hub) Target {
	select { //nolint:staticcheck // This fixture proves one-case select source identity.
	case target := <-hub.Wake:
		return target
	}
}
