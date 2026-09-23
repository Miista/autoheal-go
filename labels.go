package main

import "strings"

// Label values arrive as strings whatever the author wrote, and
// docker-compose coerces unquoted YAML booleans on the way: `autoheal: true`
// reaches Docker as "True", `autoheal: "true"` as "true". Both are the same
// intent and both are accepted here.
//
// The point of parsing rather than comparing is the third case. A label that
// is present but unrecognised -- "ture", "yes please", "1.0" -- is a mistake
// somebody made while trying to say something, and the old behaviour of
// falling through to the default made it indistinguishable from having said
// nothing at all. Upstream's own README documents `autoheal=false` for
// exclusion while its code only ever tested for "False", so a user following
// the documentation got silence and no exclusion (upstream/126, upstream/73);
// upstream/119 is the same shape with `autoheal: ture` typed literally.
// Returning ok=false lets the caller say so and leave the container alone.
//
// Absence is not a fault. A container with no label at all has expressed no
// intent, and in AUTOHEAL_CONTAINER_LABEL=all mode the default still applies
// to it -- callers check presence before asking this.
func parseLabelBool(v string) (value, ok bool) {
	switch v {
	case "true", "True", "TRUE", "1", "yes", "Yes", "YES", "on", "On", "ON":
		return true, true
	case "false", "False", "FALSE", "0", "no", "No", "NO", "off", "Off", "OFF":
		return false, true
	default:
		return false, false
	}
}

// nameOf is the best label available for a container in a log line, used on
// paths that run before the caller has established the container still exists.
// A container can legitimately reach us with no name -- it was removed between
// the daemon building the list and us reading it -- and a misconfiguration is
// still worth reporting against its id when that happens.
func nameOf(c containerSummary) string {
	if len(c.Names) == 0 {
		return shortID(c.ID)
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// labelName is the label key to read off a container.
//
// AUTOHEAL_CONTAINER_LABEL carries two different meanings in one variable: it
// is normally the name of the label to look for, but the reserved value "all"
// is a mode rather than a name -- watch everything by default. The label
// itself is still read in that mode, because "all" is only a default and
// autoheal=false has to be able to override it (upstream/126, upstream/73),
// so the key to read is the conventional "autoheal" rather than the sentinel.
func (c config) labelName() string {
	if c.ContainerLabel == "all" {
		return "autoheal"
	}
	return c.ContainerLabel
}

// healMode is how a container asks to be healed: restarted in place, or
// destroyed and rebuilt from its compose file.
//
// Restart is the default and the only mode upstream has. It is `docker
// restart` -- stop and start the same container object -- and it is the right
// answer for a process that has wedged.
//
// Recreate exists because restart cannot fix a container whose configuration
// has gone stale, and the case that forced it is network_mode:
// service:<vpn-container>. Docker resolves that to a concrete container ID
// once, at creation, and stores it; when the VPN container is recreated the ID
// it points at no longer exists. A restart replays the same stored ID and
// fails with "No such container" every time -- observed as qbittorrent sitting
// at Exited(128) while autoheal retried and got nowhere. Only rebuilding the
// container from the compose file re-resolves the peer.
type healMode int

const (
	healRestart healMode = iota
	healRecreate
)

func (m healMode) String() string {
	if m == healRecreate {
		return "recreate"
	}
	return "restart"
}

// parseHealMode reads the autoheal.mode label. Absent means restart, which
// keeps every existing compose file behaving exactly as it did.
//
// An unreadable value is refused rather than defaulted, for the same reason
// parseLabelBool refuses one: somebody wrote it meaning something, and
// quietly restarting a container whose owner asked for a recreate would
// reproduce the exact failure the label exists to fix.
func parseHealMode(v string) (mode healMode, ok bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "restart":
		return healRestart, true
	case "recreate":
		return healRecreate, true
	default:
		return healRestart, false
	}
}

// composeProject is the compose invocation recovered from a container's own
// labels. Compose stamps these at creation, so a container carries everything
// needed to rebuild it and autoheal needs no configuration of its own -- no
// project path env var, no per-container settings.
type composeProject struct {
	Name        string
	WorkingDir  string
	ConfigFiles []string
	Service     string
}

// composeProjectFrom recovers the compose invocation from container labels.
//
// ConfigFiles is documented as comma-separated when a project has several
// files; a single-file project is the common case and `include:` is resolved
// by compose rather than listed here.
//
// Missing labels mean the container was not created by compose -- a plain
// `docker run` -- and there is no compose file to rebuild it from. That is
// reported rather than guessed at.
func composeProjectFrom(labels map[string]string) (composeProject, bool) {
	p := composeProject{
		Name:       labels["com.docker.compose.project"],
		WorkingDir: labels["com.docker.compose.project.working_dir"],
		Service:    labels["com.docker.compose.service"],
	}
	for _, f := range strings.Split(labels["com.docker.compose.project.config_files"], ",") {
		if f = strings.TrimSpace(f); f != "" {
			p.ConfigFiles = append(p.ConfigFiles, f)
		}
	}
	if p.Name == "" || p.WorkingDir == "" || p.Service == "" || len(p.ConfigFiles) == 0 {
		return composeProject{}, false
	}
	return p, true
}

// args builds the compose command line. The paths are the host's, taken
// verbatim from the labels, which is why the project directory must be bind
// mounted into this container at the identical path -- see recreate().
func (p composeProject) args() []string {
	args := []string{"--project-name", p.Name, "--project-directory", p.WorkingDir}
	for _, f := range p.ConfigFiles {
		args = append(args, "-f", f)
	}
	return append(args, "up", "-d", "--force-recreate", p.Service)
}
