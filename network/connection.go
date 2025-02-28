package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/tevino/abool"

	"github.com/safing/portbase/database/record"
	"github.com/safing/portbase/log"
	"github.com/safing/portmaster/intel"
	"github.com/safing/portmaster/netenv"
	"github.com/safing/portmaster/network/netutils"
	"github.com/safing/portmaster/network/packet"
	"github.com/safing/portmaster/process"
	_ "github.com/safing/portmaster/process/tags"
	"github.com/safing/portmaster/resolver"
	"github.com/safing/spn/access"
	"github.com/safing/spn/access/account"
	"github.com/safing/spn/navigator"
)

// FirewallHandler defines the function signature for a firewall
// handle function. A firewall handler is responsible for finding
// a reasonable verdict for the connection conn. The connection is
// locked before the firewall handler is called.
type FirewallHandler func(conn *Connection, pkt packet.Packet)

// ProcessContext holds additional information about the process
// that initiated a connection.
type ProcessContext struct {
	// ProcessName is the name of the process.
	ProcessName string
	// ProfileName is the name of the profile.
	ProfileName string
	// BinaryPath is the path to the process binary.
	BinaryPath string
	// CmdLine holds the execution parameters.
	CmdLine string
	// PID is the process identifier.
	PID int
	// CreatedAt the time when the process was created.
	CreatedAt int64
	// Profile is the ID of the main profile that
	// is applied to the process.
	Profile string
	// Source is the source of the profile.
	Source string
}

// ConnectionType is a type of connection.
type ConnectionType int8

// Connection Types.
const (
	Undefined ConnectionType = iota
	IPConnection
	DNSRequest
)

// Connection describes a distinct physical network connection
// identified by the IP/Port pair.
type Connection struct { //nolint:maligned // TODO: fix alignment
	record.Base
	sync.Mutex

	// ID holds a unique request/connection id and is considered immutable after
	// creation.
	ID string
	// Type defines the connection type.
	Type ConnectionType
	// External defines if the connection represents an external request or
	// connection.
	External bool
	// Scope defines the scope of a connection. For DNS requests, the
	// scope is always set to the domain name. For direct packet
	// connections the scope consists of the involved network environment
	// and the packet direction. Once a connection object is created,
	// Scope is considered immutable.
	// Deprecated: This field holds duplicate information, which is accessible
	// clearer through other attributes. Please use conn.Type, conn.Inbound
	// and conn.Entity.Domain instead.
	Scope string
	// IPVersion is set to the packet IP version. It is not set (0) for
	// connections created from a DNS request.
	IPVersion packet.IPVersion
	// Inbound is set to true if the connection is incoming. Inbound is
	// only set when a connection object is created and is considered
	// immutable afterwards.
	Inbound bool
	// IPProtocol is set to the transport protocol used by the connection.
	// Is is considered immutable once a connection object has been
	// created. IPProtocol is not set for connections that have been
	// created from a DNS request.
	IPProtocol packet.IPProtocol
	// LocalIP holds the local IP address of the connection. It is not
	// set for connections created from DNS requests. LocalIP is
	// considered immutable once a connection object has been created.
	LocalIP net.IP
	// LocalIPScope holds the network scope of the local IP.
	LocalIPScope netutils.IPScope
	// LocalPort holds the local port of the connection. It is not
	// set for connections created from DNS requests. LocalPort is
	// considered immutable once a connection object has been created.
	LocalPort uint16
	// PID holds the PID of the owning process.
	PID int
	// Entity describes the remote entity that the connection has been
	// established to. The entity might be changed or information might
	// be added to it during the livetime of a connection. Access to
	// entity must be guarded by the connection lock.
	Entity *intel.Entity
	// Resolver holds information about the resolver used to resolve
	// Entity.Domain.
	Resolver *resolver.ResolverInfo
	// Verdict holds the decisions that are made for a connection
	// The verdict may change so any access to it must be guarded by the
	// connection lock.
	Verdict struct {
		// Worst verdict holds the worst verdict that was assigned to this
		// connection from a privacy/security perspective.
		Worst Verdict
		// Active verdict holds the verdict that Portmaster will respond with.
		// This is different from the Firewall verdict in order to guarantee proper
		// transition between verdicts that need the connection to be re-established.
		Active Verdict
		// Firewall holds the last (most recent) decision by the firewall.
		Firewall Verdict
	}
	// Reason holds information justifying the verdict, as well as additional
	// information about the reason.
	// Access to Reason must be guarded by the connection lock.
	Reason Reason
	// Started holds the number of seconds in UNIX epoch time at which
	// the connection has been initiated and first seen by the portmaster.
	// Started is only ever set when creating a new connection object
	// and is considered immutable afterwards.
	Started int64
	// Ended is set to the number of seconds in UNIX epoch time at which
	// the connection is considered terminated. Ended may be set at any
	// time so access must be guarded by the connection lock.
	Ended int64
	// VerdictPermanent is set to true if the final verdict is permanent
	// and the connection has been (or will be) handed back to the kernel.
	// VerdictPermanent may be changed together with the Verdict and Reason
	// properties and must be guarded using the connection lock.
	VerdictPermanent bool
	// Inspecting is set to true if the connection is being inspected
	// by one or more of the registered inspectors. This property may
	// be changed during the lifetime of a connection and must be guarded
	// using the connection lock.
	Inspecting bool
	// Tunneled is set to true when the connection has been routed through the
	// SPN.
	Tunneled bool
	// Encrypted is currently unused and MUST be ignored.
	Encrypted bool
	// TunnelOpts holds options for tunneling the connection.
	TunnelOpts *navigator.Options
	// ProcessContext holds additional information about the process
	// that initiated the connection. It is set once when the connection
	// object is created and is considered immutable afterwards.
	ProcessContext ProcessContext
	// DNSContext holds additional information about the DNS request that was
	// probably used to resolve the IP of this connection.
	DNSContext *resolver.DNSRequestContext
	// TunnelContext holds additional information about the tunnel that this
	// connection is using.
	TunnelContext interface {
		GetExitNodeID() string
		StopTunnel() error
	}

	// HistoryEnabled is set to true when the connection should be persisted
	// in the history database.
	HistoryEnabled bool
	// BanwidthEnabled is set to true if connection bandwidth data should be persisted
	// in netquery.
	BandwidthEnabled bool

	// BytesReceived holds the observed received bytes of the connection.
	BytesReceived uint64
	// BytesSent holds the observed sent bytes of the connection.
	BytesSent uint64

	// pkgQueue is used to serialize packet handling for a single
	// connection and is served by the connections packetHandler.
	pktQueue chan packet.Packet
	// pktQueueActive signifies whether the packet queue is active and may be written to.
	pktQueueActive bool
	// pktQueueLock locks access to pktQueueActive and writing to pktQueue.
	pktQueueLock sync.Mutex

	// dataComplete signifies that all information about the connection is
	// available and an actual packet has been seen.
	// As long as this flag is not set, the connection may not be evaluated for
	// a verdict and may not be sent to the UI.
	dataComplete *abool.AtomicBool
	// Internal is set to true if the connection is attributed as an
	// Portmaster internal connection. Internal may be set at different
	// points and access to it must be guarded by the connection lock.
	Internal bool
	// process holds a reference to the actor process. That is, the
	// process instance that initiated the connection.
	process *process.Process
	// firewallHandler is the firewall handler that is called for
	// each packet sent to pktQueue.
	firewallHandler FirewallHandler
	// saveWhenFinished can be set to true during the life-time of
	// a connection and signals the firewallHandler that a Save()
	// should be issued after processing the connection.
	saveWhenFinished bool
	// activeInspectors is a slice of booleans where each entry
	// maps to the index of an available inspector. If the value
	// is true the inspector is currently active. False indicates
	// that the inspector has finished and should be skipped.
	activeInspectors []bool
	// inspectorData holds additional meta data for the inspectors.
	// using the inspectors index as a map key.
	inspectorData map[uint8]interface{}
	// ProfileRevisionCounter is used to track changes to the process
	// profile and required for correct re-evaluation of a connections
	// verdict.
	ProfileRevisionCounter uint64
	// addedToMetrics signifies if the connection has already been counted in
	// the metrics.
	addedToMetrics bool
}

// Reason holds information justifying a verdict, as well as additional
// information about the reason.
type Reason struct {
	// Msg is a human readable description of the reason.
	Msg string
	// OptionKey is the configuration option key of the setting that
	// was responsible for the verdict.
	OptionKey string
	// Profile is the database key of the profile that held the setting
	// that was responsible for the verdict.
	Profile string
	// ReasonContext may hold additional reason-specific information and
	// any access must be guarded by the connection lock.
	Context interface{}
}

func getProcessContext(ctx context.Context, proc *process.Process) ProcessContext {
	// Gather process information.
	pCtx := ProcessContext{
		ProcessName: proc.Name,
		BinaryPath:  proc.Path,
		CmdLine:     proc.CmdLine,
		PID:         proc.Pid,
		CreatedAt:   proc.CreatedAt,
	}

	// Get local profile.
	localProfile := proc.Profile().LocalProfile()
	if localProfile == nil {
		log.Tracer(ctx).Warningf("network: process %s has no profile", proc)
		return pCtx
	}

	// Add profile information and return.
	pCtx.ProfileName = localProfile.Name
	pCtx.Profile = localProfile.ID
	pCtx.Source = string(localProfile.Source)
	return pCtx
}

// NewConnectionFromDNSRequest returns a new connection based on the given dns request.
func NewConnectionFromDNSRequest(ctx context.Context, fqdn string, cnames []string, connID string, localIP net.IP, localPort uint16) *Connection {
	// Determine IP version.
	ipVersion := packet.IPv6
	if localIP.To4() != nil {
		ipVersion = packet.IPv4
	}

	// Create packet info for dns request connection.
	pi := &packet.Info{
		Inbound:  false, // outbound as we are looking for the process of the source address
		Version:  ipVersion,
		Protocol: packet.UDP,
		Src:      localIP,   // source as in the process we are looking for
		SrcPort:  localPort, // source as in the process we are looking for
		Dst:      nil,       // do not record direction
		DstPort:  0,         // do not record direction
		PID:      process.UndefinedProcessID,
	}

	// Check if the dns request connection was reported with process info.
	var proc *process.Process
	dnsRequestConn, ok := GetDNSRequestConnection(pi)
	switch {
	case !ok:
		// No dns request connection found.
	case dnsRequestConn.PID < 0:
		// Process is not identified or is special.
	case dnsRequestConn.Ended > 0 && dnsRequestConn.Ended < time.Now().Unix()-3:
		// Connection has already ended (too long ago).
		log.Tracer(ctx).Debugf("network: found ended dns request connection %s for dns request for %s", dnsRequestConn, fqdn)
	default:
		log.Tracer(ctx).Debugf("network: found matching dns request connection %s", dnsRequestConn.String())
		// Inherit PID.
		pi.PID = dnsRequestConn.PID
		// Inherit process struct itself, as the PID may already be re-used.
		proc = dnsRequestConn.process
	}

	// Find process by remote IP/Port.
	if pi.PID == process.UndefinedProcessID {
		pi.PID, _, _ = process.GetPidOfConnection(
			ctx,
			pi,
		)
	}

	// Get process and profile with PID.
	if proc == nil {
		proc, _ = process.GetProcessWithProfile(ctx, pi.PID)
	}

	timestamp := time.Now().Unix()
	dnsConn := &Connection{
		ID:    connID,
		Type:  DNSRequest,
		Scope: fqdn,
		PID:   proc.Pid,
		Entity: &intel.Entity{
			Domain:  fqdn,
			CNAME:   cnames,
			IPScope: netutils.Global, // Assign a global IP scope as default.
		},
		process:        proc,
		ProcessContext: getProcessContext(ctx, proc),
		Started:        timestamp,
		Ended:          timestamp,
		dataComplete:   abool.NewBool(true),
	}

	// Inherit internal status of profile.
	if localProfile := proc.Profile().LocalProfile(); localProfile != nil {
		dnsConn.Internal = localProfile.Internal

		if err := dnsConn.UpdateFeatures(); err != nil && !errors.Is(err, access.ErrNotLoggedIn) {
			log.Tracer(ctx).Warningf("network: failed to check for enabled features: %s", err)
		}
	}

	// DNS Requests are saved by the nameserver depending on the result of the
	// query. Blocked requests are saved immediately, accepted ones are only
	// saved if they are not "used" by a connection.

	dnsConn.UpdateMeta()
	return dnsConn
}

// NewConnectionFromExternalDNSRequest returns a connection for an external DNS request.
func NewConnectionFromExternalDNSRequest(ctx context.Context, fqdn string, cnames []string, connID string, remoteIP net.IP) (*Connection, error) {
	remoteHost, err := process.GetNetworkHost(ctx, remoteIP)
	if err != nil {
		return nil, err
	}

	timestamp := time.Now().Unix()
	dnsConn := &Connection{
		ID:       connID,
		Type:     DNSRequest,
		External: true,
		Scope:    fqdn,
		PID:      process.NetworkHostProcessID,
		Entity: &intel.Entity{
			Domain:  fqdn,
			CNAME:   cnames,
			IPScope: netutils.Global, // Assign a global IP scope as default.
		},
		process:        remoteHost,
		ProcessContext: getProcessContext(ctx, remoteHost),
		Started:        timestamp,
		Ended:          timestamp,
		dataComplete:   abool.NewBool(true),
	}

	// Inherit internal status of profile.
	if localProfile := remoteHost.Profile().LocalProfile(); localProfile != nil {
		dnsConn.Internal = localProfile.Internal

		if err := dnsConn.UpdateFeatures(); err != nil && !errors.Is(err, access.ErrNotLoggedIn) {
			log.Tracer(ctx).Warningf("network: failed to check for enabled features: %s", err)
		}
	}

	// DNS Requests are saved by the nameserver depending on the result of the
	// query. Blocked requests are saved immediately, accepted ones are only
	// saved if they are not "used" by a connection.

	dnsConn.UpdateMeta()
	return dnsConn, nil
}

var tooOldTimestamp = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

// NewIncompleteConnection creates a new incomplete connection with only minimal information.
func NewIncompleteConnection(pkt packet.Packet) *Connection {
	info := pkt.Info()

	// Create new connection object.
	// We do not yet know the direction of the connection for sure, so we can only set minimal information.
	conn := &Connection{
		ID:           pkt.GetConnectionID(),
		Type:         IPConnection,
		IPVersion:    info.Version,
		IPProtocol:   info.Protocol,
		Started:      info.SeenAt.Unix(),
		PID:          info.PID,
		dataComplete: abool.NewBool(false),
	}

	// Bullshit check Started timestamp.
	if conn.Started < tooOldTimestamp {
		// Fix timestamp, use current time as fallback.
		conn.Started = time.Now().Unix()
	}

	// Save connection to internal state in order to mitigate creation of
	// duplicates. Do not propagate yet, as data is not yet complete.
	conn.UpdateMeta()
	conns.add(conn)

	return conn
}

// GatherConnectionInfo gathers information on the process and remote entity.
func (conn *Connection) GatherConnectionInfo(pkt packet.Packet) (err error) {
	// Create remote entity.
	if conn.Entity == nil {
		// Remote
		conn.Entity = (&intel.Entity{
			IP:       pkt.Info().RemoteIP(),
			Protocol: uint8(pkt.Info().Protocol),
			Port:     pkt.Info().RemotePort(),
		}).Init(pkt.Info().DstPort)

		// Local
		conn.SetLocalIP(pkt.Info().LocalIP())
		conn.LocalPort = pkt.Info().LocalPort()

		if conn.Inbound {
			switch conn.Entity.IPScope {
			case netutils.HostLocal:
				conn.Scope = IncomingHost
			case netutils.LinkLocal, netutils.SiteLocal, netutils.LocalMulticast:
				conn.Scope = IncomingLAN
			case netutils.Global, netutils.GlobalMulticast:
				conn.Scope = IncomingInternet

			case netutils.Undefined, netutils.Invalid:
				fallthrough
			default:
				conn.Scope = IncomingInvalid
			}
		} else {
			// Outbound direct (possibly P2P) connection.
			switch conn.Entity.IPScope {
			case netutils.HostLocal:
				conn.Scope = PeerHost
			case netutils.LinkLocal, netutils.SiteLocal, netutils.LocalMulticast:
				conn.Scope = PeerLAN
			case netutils.Global, netutils.GlobalMulticast:
				conn.Scope = PeerInternet

			case netutils.Undefined, netutils.Invalid:
				fallthrough
			default:
				conn.Scope = PeerInvalid
			}
		}
	}

	// Get PID if not yet available.
	if conn.PID == process.UndefinedProcessID {
		// Get process by looking at the system state tables.
		// Apply direction as reported from the state tables.
		conn.PID, conn.Inbound, _ = process.GetPidOfConnection(pkt.Ctx(), pkt.Info())
		// Errors are informational and are logged to the context.
	}

	// Only get process and profile with first real packet.
	// TODO: Remove when we got full VM/Docker support.
	if pkt.InfoOnly() {
		return nil
	}

	// Get Process and Profile.
	if conn.process == nil {
		conn.process, err = process.GetProcessWithProfile(pkt.Ctx(), conn.PID)
		// Errors are informational and are logged to the context.
		if err != nil {
			if pkt.InfoOnly() {
				conn.process = nil // Try again with real packet.
				log.Tracer(pkt.Ctx()).Debugf("network: failed to get process and profile of PID %d: %s", conn.PID, err)
			} else {
				log.Tracer(pkt.Ctx()).Warningf("network: failed to get process and profile of PID %d: %s", conn.PID, err)
			}
		}
	}

	// Apply process/profile info to connection.
	if conn.ProfileRevisionCounter == 0 && conn.process != nil {
		// Add process/profile metadata for connection.
		conn.ProcessContext = getProcessContext(pkt.Ctx(), conn.process)
		conn.ProfileRevisionCounter = conn.process.Profile().RevisionCnt()

		// Inherit internal status of profile.
		if localProfile := conn.process.Profile().LocalProfile(); localProfile != nil {
			conn.Internal = localProfile.Internal

			if err := conn.UpdateFeatures(); err != nil && !errors.Is(err, access.ErrNotLoggedIn) {
				log.Tracer(pkt.Ctx()).Warningf("network: connection %s failed to check for enabled features: %s", conn, err)
			}
		}
	}

	// Find domain and DNS context of entity.
	if conn.Entity.Domain == "" && conn.process.Profile() != nil {
		// check if we can find a domain for that IP
		ipinfo, err := resolver.GetIPInfo(conn.process.Profile().LocalProfile().ID, pkt.Info().RemoteIP().String())
		if err != nil {
			// Try again with the global scope, in case DNS went through the system resolver.
			ipinfo, err = resolver.GetIPInfo(resolver.IPInfoProfileScopeGlobal, pkt.Info().RemoteIP().String())
		}
		if err == nil {
			lastResolvedDomain := ipinfo.MostRecentDomain()
			if lastResolvedDomain != nil {
				conn.Scope = lastResolvedDomain.Domain
				conn.Entity.Domain = lastResolvedDomain.Domain
				conn.Entity.CNAME = lastResolvedDomain.CNAMEs
				conn.DNSContext = lastResolvedDomain.DNSRequestContext
				conn.Resolver = lastResolvedDomain.Resolver
				removeOpenDNSRequest(conn.process.Pid, lastResolvedDomain.Domain)
			}
		}
	}

	// Check if destination IP is the captive portal's IP.
	if conn.Entity.Domain == "" {
		portal := netenv.GetCaptivePortal()
		if pkt.Info().RemoteIP().Equal(portal.IP) {
			conn.Scope = portal.Domain
			conn.Entity.Domain = portal.Domain
		}
	}

	// Check if we have all required data for a complete packet.
	switch {
	case pkt.InfoOnly():
		// We need a full packet.
	case conn.process == nil:
		// We need a process.
	case conn.process.Profile() == nil:
		// We need a profile.
	case conn.Entity == nil:
		// We need an entity.
	default:
		// Data is complete!
		conn.dataComplete.Set()
	}

	return nil
}

// GetConnection fetches a Connection from the database.
func GetConnection(connID string) (*Connection, bool) {
	return conns.get(connID)
}

// GetAllConnections Gets all connection.
func GetAllConnections() []*Connection {
	return conns.list()
}

// GetDNSConnection fetches a DNS Connection from the database.
func GetDNSConnection(dnsConnID string) (*Connection, bool) {
	return dnsConns.get(dnsConnID)
}

// SetLocalIP sets the local IP address together with its network scope. The
// connection is not locked for this.
func (conn *Connection) SetLocalIP(ip net.IP) {
	conn.LocalIP = ip
	conn.LocalIPScope = netutils.GetIPScope(ip)
}

// UpdateFeatures checks which connection related features may and should be
// used and sets the flags accordingly.
// The caller must hold a lock on the connection.
func (conn *Connection) UpdateFeatures() error {
	// Get user.
	user, err := access.GetUser()
	if err != nil && !errors.Is(err, access.ErrNotLoggedIn) {
		return err
	}
	// Caution: user may be nil!

	// Check if history may be used and if it is enabled for this application.
	conn.HistoryEnabled = false
	switch {
	case conn.Internal:
		// Do not record internal connections, as they are of low interest in the history.
		// TODO: Should we create a setting for this?
	case conn.Entity.IPScope.IsLocalhost():
		// Do not record localhost-only connections, as they are very low interest in the history.
		// TODO: Should we create a setting for this?
	case user.MayUse(account.FeatureHistory):
		// Check if history may be used and is enabled.
		lProfile := conn.Process().Profile()
		if lProfile != nil {
			conn.HistoryEnabled = lProfile.EnableHistory()
		}
	}

	// Check if bandwidth visibility may be used.
	conn.BandwidthEnabled = user.MayUse(account.FeatureBWVis)

	return nil
}

// AcceptWithContext accepts the connection.
func (conn *Connection) AcceptWithContext(reason, reasonOptionKey string, ctx interface{}) {
	if !conn.SetVerdict(VerdictAccept, reason, reasonOptionKey, ctx) {
		log.Warningf("filter: tried to accept %s, but current verdict is %s", conn, conn.Verdict)
	}
}

// Accept is like AcceptWithContext but only accepts a reason.
func (conn *Connection) Accept(reason, reasonOptionKey string) {
	conn.AcceptWithContext(reason, reasonOptionKey, nil)
}

// BlockWithContext blocks the connection.
func (conn *Connection) BlockWithContext(reason, reasonOptionKey string, ctx interface{}) {
	if !conn.SetVerdict(VerdictBlock, reason, reasonOptionKey, ctx) {
		log.Warningf("filter: tried to block %s, but current verdict is %s", conn, conn.Verdict)
	}
}

// Block is like BlockWithContext but does only accepts a reason.
func (conn *Connection) Block(reason, reasonOptionKey string) {
	conn.BlockWithContext(reason, reasonOptionKey, nil)
}

// DropWithContext drops the connection.
func (conn *Connection) DropWithContext(reason, reasonOptionKey string, ctx interface{}) {
	if !conn.SetVerdict(VerdictDrop, reason, reasonOptionKey, ctx) {
		log.Warningf("filter: tried to drop %s, but current verdict is %s", conn, conn.Verdict)
	}
}

// Drop is like DropWithContext but does only accepts a reason.
func (conn *Connection) Drop(reason, reasonOptionKey string) {
	conn.DropWithContext(reason, reasonOptionKey, nil)
}

// DenyWithContext blocks or drops the link depending on the connection direction.
func (conn *Connection) DenyWithContext(reason, reasonOptionKey string, ctx interface{}) {
	if conn.Inbound {
		conn.DropWithContext(reason, reasonOptionKey, ctx)
	} else {
		conn.BlockWithContext(reason, reasonOptionKey, ctx)
	}
}

// Deny is like DenyWithContext but only accepts a reason.
func (conn *Connection) Deny(reason, reasonOptionKey string) {
	conn.DenyWithContext(reason, reasonOptionKey, nil)
}

// FailedWithContext marks the connection with VerdictFailed and stores the reason.
func (conn *Connection) FailedWithContext(reason, reasonOptionKey string, ctx interface{}) {
	if !conn.SetVerdict(VerdictFailed, reason, reasonOptionKey, ctx) {
		log.Warningf("filter: tried to drop %s due to error but current verdict is %s", conn, conn.Verdict)
	}
}

// Failed is like FailedWithContext but only accepts a string.
func (conn *Connection) Failed(reason, reasonOptionKey string) {
	conn.FailedWithContext(reason, reasonOptionKey, nil)
}

// SetVerdict sets a new verdict for the connection.
func (conn *Connection) SetVerdict(newVerdict Verdict, reason, reasonOptionKey string, reasonCtx interface{}) (ok bool) {
	conn.SetVerdictDirectly(newVerdict)

	// Set reason and context.
	conn.Reason.Msg = reason
	conn.Reason.Context = reasonCtx

	// Reset option key.
	conn.Reason.OptionKey = ""
	conn.Reason.Profile = ""

	// Set option key if data is available.
	if reasonOptionKey != "" {
		lp := conn.Process().Profile()
		if lp != nil {
			conn.Reason.OptionKey = reasonOptionKey
			conn.Reason.Profile = lp.GetProfileSource(conn.Reason.OptionKey)
		}
	}

	return true // TODO: remove
}

// SetVerdictDirectly sets the firewall verdict.
func (conn *Connection) SetVerdictDirectly(newVerdict Verdict) {
	conn.Verdict.Firewall = newVerdict
}

// VerdictVerb returns the verdict as a verb, while taking any special states
// into account.
func (conn *Connection) VerdictVerb() string {
	if conn.Verdict.Firewall == conn.Verdict.Active {
		return conn.Verdict.Firewall.Verb()
	}
	return fmt.Sprintf(
		"%s (transitioning to %s)",
		conn.Verdict.Active.Verb(),
		conn.Verdict.Firewall.Verb(),
	)
}

// DataIsComplete returns whether all information about the connection is
// available and an actual packet has been seen.
// As long as this flag is not set, the connection may not be evaluated for
// a verdict and may not be sent to the UI.
func (conn *Connection) DataIsComplete() bool {
	return conn.dataComplete.IsSet()
}

// Process returns the connection's process.
func (conn *Connection) Process() *process.Process {
	return conn.process
}

// SaveWhenFinished marks the connection for saving it after the firewall handler.
func (conn *Connection) SaveWhenFinished() {
	conn.saveWhenFinished = true
}

// Save saves the connection in the storage and propagates the change
// through the database system. Save may lock dnsConnsLock or connsLock
// in if Save() is called the first time.
// Callers must make sure to lock the connection itself before calling
// Save().
func (conn *Connection) Save() {
	conn.UpdateMeta()

	// Do not save/update until data is complete.
	if !conn.DataIsComplete() {
		return
	}

	if !conn.KeyIsSet() {
		if conn.Type == DNSRequest {
			conn.SetKey(makeKey(conn.process.Pid, dbScopeDNS, conn.ID))
			dnsConns.add(conn)
		} else {
			conn.SetKey(makeKey(conn.process.Pid, dbScopeIP, conn.ID))
			conns.add(conn)
		}
	}

	conn.addToMetrics()

	// notify database controller
	dbController.PushUpdate(conn)
}

// delete deletes a link from the storage and propagates the change.
// delete may lock either the dnsConnsLock or connsLock. Callers
// must still make sure to lock the connection itself.
func (conn *Connection) delete() {
	// A connection without an ID has been created from
	// a DNS request rather than a packet. Choose the correct
	// connection store here.
	if conn.Type == IPConnection {
		conns.delete(conn)
	} else {
		dnsConns.delete(conn)
	}

	conn.Meta().Delete()

	// Notify database controller if data is complete and thus connection was previously exposed.
	if conn.DataIsComplete() {
		dbController.PushUpdate(conn)
	}
}

// SetFirewallHandler sets the firewall handler for this link, and starts a
// worker to handle the packets.
// The caller needs to hold a lock on the connection.
// Cannot be called with "nil" handler. Call StopFirewallHandler() instead.
func (conn *Connection) SetFirewallHandler(handler FirewallHandler) {
	if handler == nil {
		return
	}

	// Initialize packet queue, if needed.
	conn.pktQueueLock.Lock()
	defer conn.pktQueueLock.Unlock()
	if !conn.pktQueueActive {
		conn.pktQueue = make(chan packet.Packet, 100)
		conn.pktQueueActive = true
	}

	// Start packet handler worker when new handler is set.
	if conn.firewallHandler == nil {
		module.StartWorker("packet handler", conn.packetHandlerWorker)
	}

	// Set new handler.
	conn.firewallHandler = handler
}

// UpdateFirewallHandler sets the firewall handler if it already set and the
// given handler is not nil.
// The caller needs to hold a lock on the connection.
func (conn *Connection) UpdateFirewallHandler(handler FirewallHandler) {
	if handler != nil && conn.firewallHandler != nil {
		conn.firewallHandler = handler
	}
}

// StopFirewallHandler unsets the firewall handler and stops the handler worker.
// The caller needs to hold a lock on the connection.
func (conn *Connection) StopFirewallHandler() {
	conn.pktQueueLock.Lock()
	defer conn.pktQueueLock.Unlock()

	// Unset the firewall handler to revert to the default handler.
	conn.firewallHandler = nil

	// Signal the packet handler worker that it can stop.
	if conn.pktQueueActive {
		close(conn.pktQueue)
		conn.pktQueueActive = false
	}

	// Unset the packet queue so that it can be freed.
	conn.pktQueue = nil
}

// HandlePacket queues packet of Link for handling.
func (conn *Connection) HandlePacket(pkt packet.Packet) {
	conn.pktQueueLock.Lock()
	defer conn.pktQueueLock.Unlock()

	// execute handler or verdict
	if conn.pktQueueActive {
		select {
		case conn.pktQueue <- pkt:
		default:
			log.Debugf(
				"filter: dropping packet %s, as there is no space in the connection's handling queue",
				pkt,
			)
			_ = pkt.Drop()
		}
	} else {
		// Run default handler.
		defaultFirewallHandler(conn, pkt)

		// Record metrics.
		packetHandlingHistogram.UpdateDuration(pkt.Info().SeenAt)
	}
}

var infoOnlyPacketsActive = abool.New()

// packetHandlerWorker sequentially handles queued packets.
func (conn *Connection) packetHandlerWorker(ctx context.Context) error {
	// Copy packet queue, so we can remove the reference from the connection
	// when we stop the firewall handler.
	var pktQueue chan packet.Packet
	func() {
		conn.pktQueueLock.Lock()
		defer conn.pktQueueLock.Unlock()
		pktQueue = conn.pktQueue
	}()

	// pktSeq counts the seen packets.
	var pktSeq int

	for {
		select {
		case pkt := <-pktQueue:
			if pkt == nil {
				return nil
			}
			pktSeq++

			// Attempt to optimize packet handling order by handling info-only packets first.
			switch {
			case pktSeq > 1:
				// Order correction is only for first packet.

			case pkt.InfoOnly():
				// Correct order only if first packet is not info-only.

				// We have observed a first packet that is info-only.
				// Info-only packets seem to be active and working.
				infoOnlyPacketsActive.Set()

			case pkt.ExpectInfo():
				// Packet itself tells us that we should expect an info-only packet.
				fallthrough

			case infoOnlyPacketsActive.IsSet() && pkt.IsOutbound():
				// Info-only packets are active and the packet is outbound.
				// The probability is high that we will also get an info-only packet for this connection.
				// TODO: Do not do this for forwarded packets in the future.

				// DEBUG:
				// log.Debugf("filter: waiting for info only packet in order to pull forward: %s", pkt)
				select {
				case infoPkt := <-pktQueue:
					if infoPkt != nil {
						// DEBUG:
						// log.Debugf("filter: packet #%d [pulled forward] info=%v PID=%d packet: %s", pktSeq, infoPkt.InfoOnly(), infoPkt.Info().PID, pkt)
						packetHandlerHandleConn(ctx, conn, infoPkt)
						pktSeq++
					}
				case <-time.After(1 * time.Millisecond):
				}
			}

			// DEBUG:
			// switch {
			// case pkt.Info().Inbound:
			// 	log.Debugf("filter: packet #%d info=%v PID=%d packet: %s", pktSeq, pkt.InfoOnly(), pkt.Info().PID, pkt)
			// case pktSeq == 1 && !pkt.InfoOnly():
			// 	log.Warningf("filter: packet #%d [should be info only!] info=%v PID=%d packet: %s", pktSeq, pkt.InfoOnly(), pkt.Info().PID, pkt)
			// case pktSeq >= 2 && pkt.InfoOnly():
			// 	log.Errorf("filter: packet #%d [should not be info only!] info=%v PID=%d packet: %s", pktSeq, pkt.InfoOnly(), pkt.Info().PID, pkt)
			// default:
			// 	log.Debugf("filter: packet #%d info=%v PID=%d packet: %s", pktSeq, pkt.InfoOnly(), pkt.Info().PID, pkt)
			// }

			packetHandlerHandleConn(ctx, conn, pkt)

		case <-ctx.Done():
			return nil
		}
	}
}

func packetHandlerHandleConn(ctx context.Context, conn *Connection, pkt packet.Packet) {
	conn.Lock()
	defer conn.Unlock()

	// Check if we should use the default handler.
	// The default handler is only for fully decided
	// connections and just applying the verdict.
	// There is no logging for these packets.
	if conn.firewallHandler == nil {
		// Run default handler.
		defaultFirewallHandler(conn, pkt)

		// Record metrics.
		packetHandlingHistogram.UpdateDuration(pkt.Info().SeenAt)

		return
	}

	// Create tracing context.
	// Add context tracer and set context on packet.
	traceCtx, tracer := log.AddTracer(ctx)
	if tracer != nil {
		// The trace is submitted in `network.Connection.packetHandler()`.
		tracer.Tracef("filter: handling packet: %s", pkt)
	}
	pkt.SetCtx(traceCtx)

	// Handle packet with set handler.
	conn.firewallHandler(conn, pkt)

	// Record metrics.
	packetHandlingHistogram.UpdateDuration(pkt.Info().SeenAt)

	// Log result and submit trace.
	switch {
	case conn.DataIsComplete():
		tracer.Infof("filter: connection %s %s: %s", conn, conn.VerdictVerb(), conn.Reason.Msg)
	case conn.Verdict.Firewall != VerdictUndecided:
		tracer.Debugf("filter: connection %s fast-tracked", pkt)
	default:
		tracer.Debugf("filter: gathered data on connection %s", conn)
	}
	// Submit trace logs.
	tracer.Submit()

	// Push changes, if there are any.
	if conn.saveWhenFinished {
		conn.saveWhenFinished = false
		conn.Save()
	}
}

// GetActiveInspectors returns the list of active inspectors.
func (conn *Connection) GetActiveInspectors() []bool {
	return conn.activeInspectors
}

// SetActiveInspectors sets the list of active inspectors.
func (conn *Connection) SetActiveInspectors(newInspectors []bool) {
	conn.activeInspectors = newInspectors
}

// GetInspectorData returns the list of inspector data.
func (conn *Connection) GetInspectorData() map[uint8]interface{} {
	return conn.inspectorData
}

// SetInspectorData set the list of inspector data.
func (conn *Connection) SetInspectorData(newInspectorData map[uint8]interface{}) {
	conn.inspectorData = newInspectorData
}

// String returns a string representation of conn.
func (conn *Connection) String() string {
	switch {
	case conn.process == nil || conn.Entity == nil:
		return conn.ID
	case conn.Inbound:
		return fmt.Sprintf("%s <- %s", conn.process, conn.Entity.IP)
	case conn.Entity.Domain != "":
		return fmt.Sprintf("%s to %s (%s)", conn.process, conn.Entity.Domain, conn.Entity.IP)
	default:
		return fmt.Sprintf("%s -> %s", conn.process, conn.Entity.IP)
	}
}
