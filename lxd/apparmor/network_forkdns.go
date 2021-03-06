package apparmor

import (
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"text/template"

	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/shared"
)

var forkdnsProfileTpl = template.Must(template.New("forkdnsProfile").Parse(`#include <tunables/global>
profile "{{ .name }}" flags=(attach_disconnected,mediate_deleted) {
  #include <abstractions/base>

  # Capabilities
  capability net_bind_service,

  # Network access
  network inet dgram,
  network inet6 dgram,

  # Network-specific paths
  {{ .varPath }}/networks/{{ .networkName }}/dnsmasq.leases r,
  {{ .varPath }}/networks/{{ .networkName }}/forkdns.servers/servers.conf r,

  # Needed for lxd fork commands
  @{PROC}/@{pid}/cmdline r,
  {{ .rootPath }}/{etc,lib,usr/lib}/os-release r,

  # Things that we definitely don't need
  deny @{PROC}/@{pid}/cgroup r,
  deny /sys/module/apparmor/parameters/enabled r,
  deny /sys/kernel/mm/transparent_hugepage/hpage_pmd_size r,

{{- if .snap }}
  # The binary itself (for nesting)
  /var/snap/lxd/common/lxd.debug      mr,
  /snap/lxd/current/bin/lxd           mr,
  /snap/lxd/*/bin/lxd                 mr,

  # Snap-specific libraries
  /snap/lxd/current/lib/**.so*            mr,
  /snap/lxd/*/lib/**.so*                  mr,
{{- end }}
}
`))

// forkdnsProfile generates the AppArmor profile template from the given network.
func forkdnsProfile(state *state.State, n network) (string, error) {
	rootPath := ""
	if shared.InSnap() {
		rootPath = "/var/lib/snapd/hostfs"
	}

	// Render the profile.
	var sb *strings.Builder = &strings.Builder{}
	err := forkdnsProfileTpl.Execute(sb, map[string]interface{}{
		"name":        ForkdnsProfileName(n),
		"networkName": n.Name(),
		"varPath":     shared.VarPath(""),
		"rootPath":    rootPath,
		"snap":        shared.InSnap(),
	})
	if err != nil {
		return "", err
	}

	return sb.String(), nil
}

// ForkdnsProfileName returns the AppArmor profile name.
func ForkdnsProfileName(n network) string {
	path := shared.VarPath("")
	name := fmt.Sprintf("%s_<%s>", n.Name(), path)

	// Max length in AppArmor is 253 chars.
	if len(name)+12 >= 253 {
		hash := sha256.New()
		io.WriteString(hash, name)
		name = fmt.Sprintf("%x", hash.Sum(nil))
	}

	return fmt.Sprintf("lxd_forkdns-%s", name)
}

// forkdnsProfileFilename returns the name of the on-disk profile name.
func forkdnsProfileFilename(n network) string {
	name := n.Name()

	// Max length in AppArmor is 253 chars.
	if len(name)+12 >= 253 {
		hash := sha256.New()
		io.WriteString(hash, name)
		name = fmt.Sprintf("%x", hash.Sum(nil))
	}

	return fmt.Sprintf("lxd_forkdns-%s", name)
}
