// Package orderroute resolves order dispatch targets from order definitions and
// city configuration.
package orderroute

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

// QualifyOrderPool resolves an order's pool target to the route used for
// dispatch and pool demand.
func QualifyOrderPool(order orders.Order, cfg *config.City) (string, error) {
	return QualifyPool(order.Pool, order.Rig, cfg, orderPoolSourceDirHint(order))
}

// QualifyPool resolves a raw pool name from an order TOML to the qualified
// agent route used by dispatcher, store routing, and API projections.
func QualifyPool(pool, rig string, cfg *config.City, sourceDirHint string) (string, error) {
	if strings.Contains(pool, "/") {
		return pool, nil
	}
	if cfg == nil {
		if rig == "" {
			return pool, nil
		}
		return rig + "/" + pool, nil
	}

	cleanHint := ""
	if sourceDirHint != "" {
		cleanHint = filepath.Clean(sourceDirHint)
	}

	if rig != "" {
		qualified, matched, err := qualifyPoolInDir(pool, rig, fmt.Sprintf("rig %q", rig), cfg, cleanHint)
		if err != nil {
			return "", err
		}
		if matched {
			return rig + "/" + qualified, nil
		}
		qualified, matched, err = qualifyPoolInDir(pool, "", "city order", cfg, cleanHint)
		if err != nil {
			return "", err
		}
		if matched {
			return qualified, nil
		}
		return rig + "/" + pool, nil
	}

	qualified, matched, err := qualifyPoolInDir(pool, "", "city order", cfg, cleanHint)
	if err != nil {
		return "", err
	}
	if matched {
		return qualified, nil
	}
	return pool, nil
}

func orderPoolSourceDirHint(order orders.Order) string {
	if order.FormulaLayer == "" {
		return ""
	}
	return filepath.Clean(filepath.Dir(order.FormulaLayer))
}

func qualifyPoolInDir(pool, dir, scope string, cfg *config.City, cleanHint string) (string, bool, error) {
	var exactQualified []string
	var sourceScopedMatches []string
	var localBareMatches []string
	var bareMatches []string
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Dir != dir {
			continue
		}
		switch {
		case strings.Contains(pool, ".") && agent.BindingQualifiedName() == pool:
			exactQualified = appendUniquePoolTarget(exactQualified, agent.BindingQualifiedName())
		case agent.Name == pool:
			bareMatches = appendUniquePoolTarget(bareMatches, agent.BindingQualifiedName())
			if agent.BindingName == "" {
				localBareMatches = appendUniquePoolTarget(localBareMatches, agent.BindingQualifiedName())
			}
			if cleanHint != "" && filepath.Clean(agent.SourceDir) == cleanHint {
				sourceScopedMatches = appendUniquePoolTarget(sourceScopedMatches, agent.BindingQualifiedName())
			}
		}
	}

	switch {
	case len(exactQualified) == 1:
		return exactQualified[0], true, nil
	case len(exactQualified) > 1:
		return "", false, fmt.Errorf("ambiguous pool %q for %s: matches %s", pool, scope, strings.Join(exactQualified, ", "))
	case len(sourceScopedMatches) == 1:
		return sourceScopedMatches[0], true, nil
	case len(sourceScopedMatches) > 1:
		return "", false, fmt.Errorf("ambiguous pool %q for %s: matches %s", pool, scope, strings.Join(sourceScopedMatches, ", "))
	case len(localBareMatches) == 1:
		return localBareMatches[0], true, nil
	case len(localBareMatches) > 1:
		return "", false, fmt.Errorf("ambiguous pool %q for %s: matches %s", pool, scope, strings.Join(localBareMatches, ", "))
	case len(bareMatches) == 1:
		return bareMatches[0], true, nil
	case len(bareMatches) > 1:
		return "", false, fmt.Errorf("ambiguous pool %q for %s: matches %s", pool, scope, strings.Join(bareMatches, ", "))
	}
	return pool, false, nil
}

func appendUniquePoolTarget(values []string, want string) []string {
	for _, existing := range values {
		if existing == want {
			return values
		}
	}
	return append(values, want)
}
