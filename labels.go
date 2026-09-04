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
