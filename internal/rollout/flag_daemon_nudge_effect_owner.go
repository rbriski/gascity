package rollout

import "github.com/gastownhall/gascity/internal/config"

// KeyDaemonNudgeEffectOwner is the exported registry key for the cold nudge
// provider-effect ownership handoff.
const KeyDaemonNudgeEffectOwner = "daemon.nudge_effect_owner"

const keyDaemonNudgeEffectOwner = KeyDaemonNudgeEffectOwner

// NudgeEffectOwner returns the boot-latched daemon.nudge_effect_owner mode.
func (f Flags) NudgeEffectOwner() Mode {
	return f.nudgeEffectOwner.value
}

// WithNudgeEffectOwner overrides daemon.nudge_effect_owner on a ForTest Flags
// value.
func WithNudgeEffectOwner(mode Mode) ForTestOption {
	return func(b *flagsBuilder) {
		b.flags.nudgeEffectOwner = resolved[Mode]{value: mode, origin: OriginConfig}
	}
}

func readDaemonNudgeEffectOwner(cfg *config.City) (raw string, defined bool) {
	raw = cfg.Daemon.NudgeEffectOwner
	return raw, raw != ""
}
