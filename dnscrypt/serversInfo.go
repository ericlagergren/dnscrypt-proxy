package dnscrypt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VividCortex/ewma"
	"github.com/jedisct1/dlog"
	stamps "github.com/jedisct1/go-dnsstamps"
	"golang.org/x/crypto/ed25519"
)

const (
	RTTEwmaDecay = 10.0
)

type RegisteredServer struct {
	Name        string
	Stamp       stamps.ServerStamp
	Description string
}

type ServerInfo struct {
	Proto              stamps.StampProtoType
	MagicQuery         [8]byte
	ServerPk           [32]byte
	SharedKey          [32]byte
	CryptoConstruction CryptoConstruction
	Name               string
	Timeout            time.Duration
	URL                *url.URL
	HostName           string
	UDPAddr            *net.UDPAddr
	TCPAddr            *net.TCPAddr
	RelayUDPAddr       *net.UDPAddr
	RelayTCPAddr       *net.TCPAddr
	lastActionTS       time.Time
	rtt                ewma.MovingAverage
	initialRtt         int
	useGet             bool
}

type LBStrategy int

const (
	LBStrategyNone = LBStrategy(iota)
	LBStrategyP2
	LBStrategyPH
	LBStrategyFirst
	LBStrategyRandom
)

const DefaultLBStrategy = LBStrategyP2

type ServersInfo struct {
	sync.RWMutex
	inner             []*ServerInfo
	registeredServers []RegisteredServer
	LBStrategy        LBStrategy
	LBEstimator       bool
}

func NewServersInfo() ServersInfo {
	return ServersInfo{LBStrategy: DefaultLBStrategy, LBEstimator: true, registeredServers: make([]RegisteredServer, 0)}
}

func (serversInfo *ServersInfo) registerServer(name string, stamp stamps.ServerStamp) error {
	newRegisteredServer := RegisteredServer{Name: name, Stamp: stamp}
	serversInfo.Lock()
	defer serversInfo.Unlock()
	for i, oldRegisteredServer := range serversInfo.registeredServers {
		if oldRegisteredServer.Name == name {
			serversInfo.registeredServers[i] = newRegisteredServer
			return nil
		}
	}
	serversInfo.registeredServers = append(serversInfo.registeredServers, newRegisteredServer)
	return nil
}

func (serversInfo *ServersInfo) refreshServer(proxy *Proxy, name string, stamp stamps.ServerStamp) error {
	serversInfo.RLock()
	isNew := true
	for _, oldServer := range serversInfo.inner {
		if oldServer.Name == name {
			isNew = false
			break
		}
	}
	serversInfo.RUnlock()
	newServer, err := fetchServerInfo(proxy, name, stamp, isNew)
	if err != nil {
		return err
	}
	if name != newServer.Name {
		dlog.Fatalf("[%s] != [%s]", name, newServer.Name)
	}
	newServer.rtt = ewma.NewMovingAverage(RTTEwmaDecay)
	newServer.rtt.Set(float64(newServer.initialRtt))
	isNew = true
	serversInfo.Lock()
	for i, oldServer := range serversInfo.inner {
		if oldServer.Name == name {
			serversInfo.inner[i] = &newServer
			isNew = false
			break
		}
	}
	if isNew {
		serversInfo.inner = append(serversInfo.inner, &newServer)
		serversInfo.registeredServers = append(serversInfo.registeredServers, RegisteredServer{Name: name, Stamp: stamp})
	}
	serversInfo.Unlock()
	return nil
}

func (serversInfo *ServersInfo) refresh(proxy *Proxy) (int, error) {
	dlog.Debug("Refreshing certificates")
	serversInfo.RLock()
	registeredServers := serversInfo.registeredServers
	serversInfo.RUnlock()
	liveServers := 0
	var err error
	for _, registeredServer := range registeredServers {
		if err = serversInfo.refreshServer(proxy, registeredServer.Name, registeredServer.Stamp); err == nil {
			liveServers++
		}
	}
	serversInfo.Lock()
	sort.SliceStable(serversInfo.inner, func(i, j int) bool {
		return serversInfo.inner[i].initialRtt < serversInfo.inner[j].initialRtt
	})
	inner := serversInfo.inner
	innerLen := len(inner)
	if innerLen > 1 {
		dlog.Notice("Sorted latencies:")
		for i := 0; i < innerLen; i++ {
			dlog.Noticef("- %5dms %s", inner[i].initialRtt, inner[i].Name)
		}
	}
	if innerLen > 0 {
		dlog.Noticef("Server with the lowest initial latency: %s (rtt: %dms)", inner[0].Name, inner[0].initialRtt)
	}
	serversInfo.Unlock()
	return liveServers, err
}

func (serversInfo *ServersInfo) estimatorUpdate() {
	// serversInfo.RWMutex is assumed to be Locked
	candidate := rand.Intn(len(serversInfo.inner))
	if candidate == 0 {
		return
	}
	candidateRtt, currentBestRtt := serversInfo.inner[candidate].rtt.Value(), serversInfo.inner[0].rtt.Value()
	if currentBestRtt < 0 {
		currentBestRtt = candidateRtt
		serversInfo.inner[0].rtt.Set(currentBestRtt)
	}
	partialSort := false
	if candidateRtt < currentBestRtt {
		serversInfo.inner[candidate], serversInfo.inner[0] = serversInfo.inner[0], serversInfo.inner[candidate]
		partialSort = true
		dlog.Debugf("New preferred candidate: %v (rtt: %d vs previous: %d)", serversInfo.inner[0].Name, int(candidateRtt), int(currentBestRtt))
	} else if candidateRtt > 0 && candidateRtt >= currentBestRtt*4.0 {
		if time.Since(serversInfo.inner[candidate].lastActionTS) > time.Duration(1*time.Minute) {
			serversInfo.inner[candidate].rtt.Add(MinF(MaxF(candidateRtt/2.0, currentBestRtt*2.0), candidateRtt))
			dlog.Debugf("Giving a new chance to candidate [%s], lowering its RTT from %d to %d (best: %d)", serversInfo.inner[candidate].Name, int(candidateRtt), int(serversInfo.inner[candidate].rtt.Value()), int(currentBestRtt))
			partialSort = true
		}
	}
	if partialSort {
		serversCount := len(serversInfo.inner)
		for i := 1; i < serversCount; i++ {
			if serversInfo.inner[i-1].rtt.Value() > serversInfo.inner[i].rtt.Value() {
				serversInfo.inner[i-1], serversInfo.inner[i] = serversInfo.inner[i], serversInfo.inner[i-1]
			}
		}
	}
}

func (serversInfo *ServersInfo) getOne() *ServerInfo {
	serversInfo.Lock()
	defer serversInfo.Unlock()
	serversCount := len(serversInfo.inner)
	if serversCount <= 0 {
		return nil
	}
	if serversInfo.LBEstimator {
		serversInfo.estimatorUpdate()
	}
	var candidate int
	switch serversInfo.LBStrategy {
	case LBStrategyFirst:
		candidate = 0
	case LBStrategyPH:
		candidate = rand.Intn(Max(Min(serversCount, 2), serversCount/2))
	case LBStrategyRandom:
		candidate = rand.Intn(serversCount)
	default:
		candidate = rand.Intn(Min(serversCount, 2))
	}
	serverInfo := serversInfo.inner[candidate]
	dlog.Debugf("Using candidate [%s] RTT: %d", (*serverInfo).Name, int((*serverInfo).rtt.Value()))

	return serverInfo
}

func fetchServerInfo(proxy *Proxy, name string, stamp stamps.ServerStamp, isNew bool) (ServerInfo, error) {
	if stamp.Proto == stamps.StampProtoTypeDNSCrypt {
		return fetchDNSCryptServerInfo(proxy, name, stamp, isNew)
	} else if stamp.Proto == stamps.StampProtoTypeDoH {
		return fetchDoHServerInfo(proxy, name, stamp, isNew)
	}
	return ServerInfo{}, errors.New("Unsupported protocol")
}

func route(proxy *Proxy, name string, stamp *stamps.ServerStamp) (*net.UDPAddr, *net.TCPAddr, error) {
	routes := proxy.Routes
	if routes == nil {
		return nil, nil, nil
	}
	relayNames, ok := (*routes)[name]
	if !ok {
		relayNames, ok = (*routes)["*"]
	}
	if !ok {
		return nil, nil, nil
	}
	var relayName string
	if len(relayNames) > 0 {
		candidate := rand.Intn(len(relayNames))
		relayName = relayNames[candidate]
	}
	var relayCandidateStamp *stamps.ServerStamp
	if len(relayName) == 0 {
		return nil, nil, fmt.Errorf("Route declared for [%v] but an empty relay list", name)
	} else if relayStamp, err := stamps.NewServerStampFromString(relayName); err == nil {
		relayCandidateStamp = &relayStamp
	} else if _, err := net.ResolveUDPAddr("udp", relayName); err == nil {
		relayCandidateStamp = &stamps.ServerStamp{
			ServerAddrStr: relayName,
			Proto:         stamps.StampProtoTypeDNSCryptRelay,
		}
	} else {
		for _, registeredServer := range proxy.RegisteredRelays {
			if registeredServer.Name == relayName {
				relayCandidateStamp = &registeredServer.Stamp
				break
			}
		}
		for _, registeredServer := range proxy.RegisteredServers {
			if registeredServer.Name == relayName {
				relayCandidateStamp = &registeredServer.Stamp
				break
			}
		}
	}
	if relayCandidateStamp == nil {
		return nil, nil, fmt.Errorf("Undefined relay [%v] for server [%v]", relayName, name)
	}
	if relayCandidateStamp.Proto == stamps.StampProtoTypeDNSCrypt ||
		relayCandidateStamp.Proto == stamps.StampProtoTypeDNSCryptRelay {
		relayUDPAddr, err := net.ResolveUDPAddr("udp", relayCandidateStamp.ServerAddrStr)
		if err != nil {
			return nil, nil, err
		}
		relayTCPAddr, err := net.ResolveTCPAddr("tcp", relayCandidateStamp.ServerAddrStr)
		if err != nil {
			return nil, nil, err
		}
		return relayUDPAddr, relayTCPAddr, nil
	}
	return nil, nil, fmt.Errorf("Invalid relay [%v] for server [%v]", relayName, name)
}

func fetchDNSCryptServerInfo(proxy *Proxy, name string, stamp stamps.ServerStamp, isNew bool) (ServerInfo, error) {
	if len(stamp.ServerPk) != ed25519.PublicKeySize {
		serverPk, err := hex.DecodeString(strings.Replace(string(stamp.ServerPk), ":", "", -1))
		if err != nil || len(serverPk) != ed25519.PublicKeySize {
			dlog.Fatalf("Unsupported public key for [%s]: [%s]", name, stamp.ServerPk)
		}
		dlog.Warnf("Public key [%s] shouldn't be hex-encoded any more", string(stamp.ServerPk))
		stamp.ServerPk = serverPk
	}
	relayUDPAddr, relayTCPAddr, err := route(proxy, name, &stamp)
	if err != nil {
		return ServerInfo{}, err
	}
	certInfo, rtt, err := FetchCurrentDNSCryptCert(proxy, &name, proxy.MainProto, stamp.ServerPk, stamp.ServerAddrStr, stamp.ProviderName, isNew, relayUDPAddr, relayTCPAddr)
	if err != nil {
		return ServerInfo{}, err
	}
	remoteUDPAddr, err := net.ResolveUDPAddr("udp", stamp.ServerAddrStr)
	if err != nil {
		return ServerInfo{}, err
	}
	remoteTCPAddr, err := net.ResolveTCPAddr("tcp", stamp.ServerAddrStr)
	if err != nil {
		return ServerInfo{}, err
	}
	return ServerInfo{
		Proto:              stamps.StampProtoTypeDNSCrypt,
		MagicQuery:         certInfo.MagicQuery,
		ServerPk:           certInfo.ServerPk,
		SharedKey:          certInfo.SharedKey,
		CryptoConstruction: certInfo.CryptoConstruction,
		Name:               name,
		Timeout:            proxy.Timeout,
		UDPAddr:            remoteUDPAddr,
		TCPAddr:            remoteTCPAddr,
		RelayUDPAddr:       relayUDPAddr,
		RelayTCPAddr:       relayTCPAddr,
		initialRtt:         rtt,
	}, nil
}

func fetchDoHServerInfo(proxy *Proxy, name string, stamp stamps.ServerStamp, isNew bool) (ServerInfo, error) {
	if len(stamp.ServerAddrStr) > 0 {
		ipOnly, _ := ExtractHostAndPort(stamp.ServerAddrStr, -1)
		if ip := ParseIP(ipOnly); ip != nil {
			proxy.XTransport.saveCachedIP(stamp.ProviderName, ip, -1*time.Second)
		}
	}
	url := &url.URL{
		Scheme: "https",
		Host:   stamp.ProviderName,
		Path:   stamp.Path,
	}
	body := []byte{
		0xca, 0xfe, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x02, 0x00, 0x01, 0x00, 0x00, 0x29, 0x10, 0x00, 0x00, 0x00, 0x80, 0x00, 0x00, 0x00,
	}
	useGet := false
	if _, _, err := proxy.XTransport.DoHQuery(useGet, url, body, proxy.Timeout); err != nil {
		useGet = true
		if _, _, err := proxy.XTransport.DoHQuery(useGet, url, body, proxy.Timeout); err != nil {
			return ServerInfo{}, err
		}
		dlog.Debugf("Server [%s] doesn't appear to support POST; falling back to GET requests", name)
	}
	resp, rtt, err := proxy.XTransport.DoHQuery(useGet, url, body, proxy.Timeout)
	if err != nil {
		return ServerInfo{}, err
	}
	tls := resp.TLS
	if tls == nil || !tls.HandshakeComplete {
		return ServerInfo{}, errors.New("TLS handshake failed")
	}
	protocol := tls.NegotiatedProtocol
	if len(protocol) == 0 {
		protocol = "h1"
		dlog.Warnf("[%s] does not support HTTP/2", name)
	}
	dlog.Infof("[%s] TLS version: %x - Protocol: %v - Cipher suite: %v", name, tls.Version, protocol, tls.CipherSuite)
	showCerts := proxy.ShowCerts
	found := false
	var wantedHash [32]byte
	for _, cert := range tls.PeerCertificates {
		h := sha256.Sum256(cert.RawTBSCertificate)
		if showCerts {
			dlog.Noticef("Advertised cert: [%s] [%x]", cert.Subject, h)
		} else {
			dlog.Debugf("Advertised cert: [%s] [%x]", cert.Subject, h)
		}
		for _, hash := range stamp.Hashes {
			if len(hash) == len(wantedHash) {
				copy(wantedHash[:], hash)
				if h == wantedHash {
					found = true
					break
				}
			}
		}
		if found {
			break
		}
	}
	if !found && len(stamp.Hashes) > 0 {
		return ServerInfo{}, fmt.Errorf("Certificate hash [%x] not found for [%s]", wantedHash, name)
	}
	respBody, err := ioutil.ReadAll(io.LimitReader(resp.Body, MaxHTTPBodyLength))
	if err != nil {
		return ServerInfo{}, err
	}
	if len(respBody) < MinDNSPacketSize || len(respBody) > MaxDNSPacketSize ||
		respBody[0] != 0xca || respBody[1] != 0xfe || respBody[4] != 0x00 || respBody[5] != 0x01 {
		return ServerInfo{}, errors.New("Webserver returned an unexpected response")
	}
	xrtt := int(rtt.Nanoseconds() / 1000000)
	if isNew {
		dlog.Noticef("[%s] OK (DoH) - rtt: %dms", name, xrtt)
	} else {
		dlog.Infof("[%s] OK (DoH) - rtt: %dms", name, xrtt)
	}
	return ServerInfo{
		Proto:      stamps.StampProtoTypeDoH,
		Name:       name,
		Timeout:    proxy.Timeout,
		URL:        url,
		HostName:   stamp.ProviderName,
		initialRtt: xrtt,
		useGet:     useGet,
	}, nil
}

func (serverInfo *ServerInfo) noticeFailure(proxy *Proxy) {
	proxy.ServersInfo.Lock()
	serverInfo.rtt.Add(float64(proxy.Timeout.Nanoseconds() / 1000000))
	proxy.ServersInfo.Unlock()
}

func (serverInfo *ServerInfo) noticeBegin(proxy *Proxy) {
	proxy.ServersInfo.Lock()
	serverInfo.lastActionTS = time.Now()
	proxy.ServersInfo.Unlock()
}

func (serverInfo *ServerInfo) noticeSuccess(proxy *Proxy) {
	now := time.Now()
	proxy.ServersInfo.Lock()
	elapsed := now.Sub(serverInfo.lastActionTS)
	elapsedMs := elapsed.Nanoseconds() / 1000000
	if elapsedMs > 0 && elapsed < proxy.Timeout {
		serverInfo.rtt.Add(float64(elapsedMs))
	}
	proxy.ServersInfo.Unlock()
}
