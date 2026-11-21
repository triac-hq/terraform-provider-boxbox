// Package client is the HTTP client of boxbox3's API (`/boxbox/api/v1/`).
// The contract is docs/api.md in the boxbox3 repository; the types here
// follow it field by field.
package client

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Phases of a machine.
const (
	PhaseCreating   = "creating"
	PhaseReady      = "ready"
	PhaseFailed     = "failed"
	PhaseDestroying = "destroying"
)

// Sources of a machine.
const (
	SourceAPI    = "api"
	SourceConfig = "config"
)

// Roles of a caller.
const (
	RoleTenant = "tenant"
	RoleAdmin  = "admin"
)

var (
	tenantRe  = regexp.MustCompile(`^[a-z][a-z0-9]{1,7}$`)
	machineRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	sizeRe    = regexp.MustCompile(`^([0-9]+)(MiB|GiB|TiB)$`)
)

// ValidTenantName tells whether the name is one the API accepts for a tenant.
func ValidTenantName(s string) bool { return tenantRe.MatchString(s) && s != "admin" }

// ValidMachineName tells whether the name is one the API accepts for a machine.
func ValidMachineName(s string) bool { return machineRe.MatchString(s) }

// Size is an amount of memory or disk, in whole mebibytes.
type Size struct{ mib int64 }

// SizeFromMiB makes a Size.
func SizeFromMiB(mib int64) Size { return Size{mib} }

// ParseSize reads the contract's form: an integer and `MiB`, `GiB` or `TiB`.
func ParseSize(s string) (Size, error) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return Size{}, fmt.Errorf("%q is not a size: an integer and MiB, GiB or TiB without a space, like \"4GiB\"", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return Size{}, fmt.Errorf("%q is not a size: the number is too large", s)
	}
	if n == 0 {
		return Size{}, fmt.Errorf("%q is not a size: zero is not allowed", s)
	}
	mul := map[string]int64{"MiB": 1, "GiB": 1024, "TiB": 1024 * 1024}[m[2]]
	if n > math.MaxInt64/mul {
		return Size{}, fmt.Errorf("%q is not a size: the number is too large", s)
	}
	return Size{n * mul}, nil
}

// MiB is the size in mebibytes.
func (s Size) MiB() int64 { return s.mib }

// WholeGiB tells whether the size is whole gibibytes.
func (s Size) WholeGiB() bool { return s.mib%1024 == 0 }

// String is the canonical form: `GiB` when whole gibibytes, otherwise `MiB`.
// Nothing at all (the usage of an empty pool) is `0GiB`.
func (s Size) String() string {
	if s.WholeGiB() {
		return strconv.FormatInt(s.mib/1024, 10) + "GiB"
	}
	return strconv.FormatInt(s.mib, 10) + "MiB"
}

// MarshalJSON writes the canonical form.
func (s Size) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// UnmarshalJSON reads any form the contract allows, and zero: a request may
// not ask for nothing, but an answer can report nothing in use.
func (s *Size) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return fmt.Errorf("a size must be a string: %w", err)
	}
	if m := sizeRe.FindStringSubmatch(str); m != nil && strings.Trim(m[1], "0") == "" {
		*s = Size{}
		return nil
	}
	v, err := ParseSize(str)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// CPU is guaranteed threads (`min`) and visible threads (`max`).
type CPU struct {
	Min float64 `json:"min"`
	Max int64   `json:"max"`
}

// Quota of a tenant.
type Quota struct {
	Machines int64 `json:"machines"`
	CPU      CPU   `json:"cpu"`
	Memory   Size  `json:"memory"`
	Disk     Size  `json:"disk"`
	HA       bool  `json:"ha"`
}

// Usage of a tenant: everything in its pool.
type Usage struct {
	Machines int64 `json:"machines"`
	CPU      CPU   `json:"cpu"`
	Memory   Size  `json:"memory"`
	Disk     Size  `json:"disk"`
}

// MeTenant is the tenant part of `GET me`.
type MeTenant struct {
	Name  string `json:"name"`
	Quota *Quota `json:"quota"`
	Usage Usage  `json:"usage"`
}

// Me is the answer of `GET me`.
type Me struct {
	User   string    `json:"user"`
	Role   string    `json:"role"`
	Tenant *MeTenant `json:"tenant"`
	Images []string  `json:"images"`
}

// HA of a machine in an answer.
type HA struct {
	Replicas  int64    `json:"replicas"`
	Nodes     []string `json:"nodes"`
	Protected bool     `json:"protected"`
	// Restart is in the answer only once the admin fields are implemented.
	Restart *bool `json:"restart,omitempty"`
}

// Addresses of a machine.
type Addresses struct {
	NAT      *string `json:"nat"`
	Internal *string `json:"internal"`
}

// PendingInt is a value the running machine does not have yet.
type PendingInt struct {
	Running    int64 `json:"running"`
	Configured int64 `json:"configured"`
}

// PendingSize is a size the running machine does not have yet.
type PendingSize struct {
	Running    Size `json:"running"`
	Configured Size `json:"configured"`
}

// Pending is what waits for the next stop and start of the machine.
type Pending struct {
	CPUMax *PendingInt  `json:"cpu_max"`
	Memory *PendingSize `json:"memory"`
}

// Machine is a machine in an answer.
type Machine struct {
	Tenant    string    `json:"tenant"`
	Name      string    `json:"name"`
	VMID      int64     `json:"vmid"`
	Source    string    `json:"source"`
	Phase     string    `json:"phase"`
	Reason    *string   `json:"reason"`
	Node      string    `json:"node"`
	Status    string    `json:"status"`
	Image     *string   `json:"image"`
	User      *string   `json:"user"`
	CPU       CPU       `json:"cpu"`
	Memory    Size      `json:"memory"`
	Disk      Size      `json:"disk"`
	HA        *HA       `json:"ha"`
	Addresses Addresses `json:"addresses"`
	Pending   *Pending  `json:"pending"`
}

// HASpec is `ha` in a request. Nodes and Restart are the owner's.
type HASpec struct {
	Replicas *int64   `json:"replicas,omitempty"`
	Nodes    []string `json:"nodes,omitempty"`
	Restart  *bool    `json:"restart,omitempty"`
}

// MachineSpec is the body of `PUT machines/{tenant}/{name}`.
// Node, User, CloudInit, ISO and HA.Nodes, HA.Restart are the owner's.
type MachineSpec struct {
	Image            string   `json:"image,omitempty"`
	CPU              *CPU     `json:"cpu,omitempty"`
	Memory           *Size    `json:"memory,omitempty"`
	Disk             *Size    `json:"disk,omitempty"`
	HA               *HASpec  `json:"ha,omitempty"`
	SSHKeys          []string `json:"ssh_keys,omitempty"`
	TailscaleAuthKey string   `json:"tailscale_auth_key,omitempty"`
	Node             string   `json:"node,omitempty"`
	User             string   `json:"user,omitempty"`
	CloudInit        string   `json:"cloud_init,omitempty"`
	ISO              string   `json:"iso,omitempty"`
}

// TenantSpec is the body of `PUT tenants/{name}`. Both keys are always sent:
// `null` takes the quota or the address away.
type TenantSpec struct {
	Quota *Quota  `json:"quota"`
	Email *string `json:"email"`
}

// Tenant is a tenant in an answer.
type Tenant struct {
	Name    string  `json:"name"`
	Email   *string `json:"email"`
	Quota   *Quota  `json:"quota"`
	Usage   Usage   `json:"usage"`
	Network string  `json:"network"`
}
