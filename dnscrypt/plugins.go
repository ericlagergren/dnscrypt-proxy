package dnscrypt

import (
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/jedisct1/dlog"
	"github.com/miekg/dns"
)

type PluginsAction int

const (
	PluginsActionNone    = 0
	PluginsActionForward = 1
	PluginsActionDrop    = 2
	PluginsActionReject  = 3
	PluginsActionSynth   = 4
)

type PluginsGlobals struct {
	sync.RWMutex
	queryPlugins           *[]Plugin
	responsePlugins        *[]Plugin
	loggingPlugins         *[]Plugin
	refusedCodeInResponses bool
	respondWithIPv4        net.IP
	respondWithIPv6        net.IP
}

type PluginsReturnCode int

const (
	PluginsReturnCodePass = iota
	PluginsReturnCodeForward
	PluginsReturnCodeDrop
	PluginsReturnCodeReject
	PluginsReturnCodeSynth
	PluginsReturnCodeParseError
	PluginsReturnCodeNXDomain
	PluginsReturnCodeResponseError
	PluginsReturnCodeServerError
	PluginsReturnCodeCloak
	PluginsReturnCodeServerTimeout
)

var PluginsReturnCodeToString = map[PluginsReturnCode]string{
	PluginsReturnCodePass:          "PASS",
	PluginsReturnCodeForward:       "FORWARD",
	PluginsReturnCodeDrop:          "DROP",
	PluginsReturnCodeReject:        "REJECT",
	PluginsReturnCodeSynth:         "SYNTH",
	PluginsReturnCodeParseError:    "PARSE_ERROR",
	PluginsReturnCodeNXDomain:      "NXDOMAIN",
	PluginsReturnCodeResponseError: "RESPONSE_ERROR",
	PluginsReturnCodeServerError:   "SERVER_ERROR",
	PluginsReturnCodeCloak:         "CLOAK",
	PluginsReturnCodeServerTimeout: "SERVER_TIMEOUT",
}

type PluginsState struct {
	sessionData                      map[string]interface{}
	action                           PluginsAction
	maxUnencryptedUDPSafePayloadSize int
	originalMaxPayloadSize           int
	maxPayloadSize                   int
	clientProto                      string
	clientAddr                       *net.Addr
	synthResponse                    *dns.Msg
	dnssec                           bool
	cacheSize                        int
	cacheNegMinTTL                   uint32
	cacheNegMaxTTL                   uint32
	cacheMinTTL                      uint32
	cacheMaxTTL                      uint32
	rejectTTL                        uint32
	questionMsg                      *dns.Msg
	requestStart                     time.Time
	requestEnd                       time.Time
	cacheHit                         bool
	returnCode                       PluginsReturnCode
	serverName                       string
}

func (proxy *Proxy) InitPluginsGlobals() error {
	queryPlugins := &[]Plugin{}

	if len(proxy.QueryMeta) != 0 {
		*queryPlugins = append(*queryPlugins, Plugin(new(PluginQueryMeta)))
	}
	if len(proxy.WhitelistNameFile) != 0 {
		*queryPlugins = append(*queryPlugins, Plugin(new(PluginWhitelistName)))
	}

	*queryPlugins = append(*queryPlugins, Plugin(new(PluginFirefox)))

	if len(proxy.BlockNameFile) != 0 {
		*queryPlugins = append(*queryPlugins, Plugin(new(PluginBlockName)))
	}
	if proxy.PluginBlockIPv6 {
		*queryPlugins = append(*queryPlugins, Plugin(new(PluginBlockIPv6)))
	}
	if len(proxy.CloakFile) != 0 {
		*queryPlugins = append(*queryPlugins, Plugin(new(PluginCloak)))
	}
	*queryPlugins = append(*queryPlugins, Plugin(new(PluginGetSetPayloadSize)))
	if proxy.Cache {
		*queryPlugins = append(*queryPlugins, Plugin(new(PluginCache)))
	}
	if len(proxy.ForwardFile) != 0 {
		*queryPlugins = append(*queryPlugins, Plugin(new(PluginForward)))
	}

	responsePlugins := &[]Plugin{}
	if len(proxy.NXLogFile) != 0 {
		*responsePlugins = append(*responsePlugins, Plugin(new(PluginNxLog)))
	}
	if len(proxy.BlockIPFile) != 0 {
		*responsePlugins = append(*responsePlugins, Plugin(new(PluginBlockIP)))
	}
	if proxy.Cache {
		*responsePlugins = append(*responsePlugins, Plugin(new(PluginCacheResponse)))
	}

	loggingPlugins := &[]Plugin{}
	if len(proxy.QueryLogFile) != 0 {
		*loggingPlugins = append(*loggingPlugins, Plugin(new(PluginQueryLog)))
	}

	for _, plugin := range *queryPlugins {
		if err := plugin.Init(proxy); err != nil {
			return err
		}
	}
	for _, plugin := range *responsePlugins {
		if err := plugin.Init(proxy); err != nil {
			return err
		}
	}
	for _, plugin := range *loggingPlugins {
		if err := plugin.Init(proxy); err != nil {
			return err
		}
	}

	proxy.pluginsGlobals.queryPlugins = queryPlugins
	proxy.pluginsGlobals.responsePlugins = responsePlugins
	proxy.pluginsGlobals.loggingPlugins = loggingPlugins

	parseBlockedQueryResponse(proxy.BlockedQueryResponse, &proxy.pluginsGlobals)

	return nil
}

// blockedQueryResponse can be 'refused', 'hinfo' or IP responses 'a:IPv4,aaaa:IPv6
func parseBlockedQueryResponse(blockedResponse string, pluginsGlobals *PluginsGlobals) {
	blockedResponse = StringStripSpaces(strings.ToLower(blockedResponse))

	if strings.HasPrefix(blockedResponse, "a:") {
		blockedIPStrings := strings.Split(blockedResponse, ",")
		(*pluginsGlobals).respondWithIPv4 = net.ParseIP(strings.TrimPrefix(blockedIPStrings[0], "a:"))

		if (*pluginsGlobals).respondWithIPv4 == nil {
			dlog.Notice("Error parsing IPv4 response given in blocked_query_response option, defaulting to `hinfo`")
			(*pluginsGlobals).refusedCodeInResponses = false
			return
		}

		if len(blockedIPStrings) > 1 {
			if strings.HasPrefix(blockedIPStrings[1], "aaaa:") {
				ipv6Response := strings.TrimPrefix(blockedIPStrings[1], "aaaa:")
				if strings.HasPrefix(ipv6Response, "[") {
					ipv6Response = strings.Trim(ipv6Response, "[]")
				}
				(*pluginsGlobals).respondWithIPv6 = net.ParseIP(ipv6Response)

				if (*pluginsGlobals).respondWithIPv6 == nil {
					dlog.Notice("Error parsing IPv6 response given in blocked_query_response option, defaulting to IPv4")
				}
			} else {
				dlog.Noticef("Invalid IPv6 response given in blocked_query_response option [%s], the option should take the form 'a:<IPv4>,aaaa:<IPv6>'", blockedIPStrings[1])
			}
		}

		if (*pluginsGlobals).respondWithIPv6 == nil {
			(*pluginsGlobals).respondWithIPv6 = (*pluginsGlobals).respondWithIPv4
		}

	} else {
		switch blockedResponse {
		case "refused":
			(*pluginsGlobals).refusedCodeInResponses = true
		case "hinfo":
			(*pluginsGlobals).refusedCodeInResponses = false
		default:
			dlog.Noticef("Invalid blocked_query_response option [%s], defaulting to `hinfo`", blockedResponse)
			(*pluginsGlobals).refusedCodeInResponses = false
		}
	}
}

type Plugin interface {
	Name() string
	Description() string
	Init(proxy *Proxy) error
	Drop() error
	Reload() error
	Eval(pluginsState *PluginsState, msg *dns.Msg) error
}

func NewPluginsState(proxy *Proxy, clientProto string, clientAddr *net.Addr, start time.Time) PluginsState {
	return PluginsState{
		action:                           PluginsActionForward,
		maxPayloadSize:                   MaxDNSUDPPacketSize - ResponseOverhead,
		clientProto:                      clientProto,
		clientAddr:                       clientAddr,
		cacheSize:                        proxy.CacheSize,
		cacheNegMinTTL:                   proxy.CacheNegMinTTL,
		cacheNegMaxTTL:                   proxy.CacheNegMaxTTL,
		cacheMinTTL:                      proxy.CacheMinTTL,
		cacheMaxTTL:                      proxy.CacheMaxTTL,
		rejectTTL:                        proxy.RejectTTL,
		questionMsg:                      nil,
		requestStart:                     start,
		maxUnencryptedUDPSafePayloadSize: MaxDNSUDPSafePacketSize,
	}
}

func (pluginsState *PluginsState) ApplyQueryPlugins(pluginsGlobals *PluginsGlobals, packet []byte, serverName string) ([]byte, error) {
	if len(*pluginsGlobals.queryPlugins) == 0 && len(*pluginsGlobals.loggingPlugins) == 0 {
		return packet, nil
	}
	pluginsState.serverName = serverName
	pluginsState.action = PluginsActionForward
	msg := dns.Msg{}
	if err := msg.Unpack(packet); err != nil {
		return packet, err
	}
	if len(msg.Question) > 1 {
		return packet, errors.New("Unexpected number of questions")
	}
	pluginsState.questionMsg = &msg
	pluginsGlobals.RLock()
	defer pluginsGlobals.RUnlock()
	for _, plugin := range *pluginsGlobals.queryPlugins {
		if err := plugin.Eval(pluginsState, &msg); err != nil {
			pluginsState.action = PluginsActionDrop
			return packet, err
		}
		if pluginsState.action == PluginsActionReject {
			synth, err := RefusedResponseFromMessage(&msg, pluginsGlobals.refusedCodeInResponses, pluginsGlobals.respondWithIPv4, pluginsGlobals.respondWithIPv6, pluginsState.rejectTTL)
			if err != nil {
				return nil, err
			}
			pluginsState.synthResponse = synth
		}
		if pluginsState.action != PluginsActionForward {
			break
		}
	}
	packet2, err := msg.PackBuffer(packet)
	if err != nil {
		return packet, err
	}
	return packet2, nil
}

func (pluginsState *PluginsState) ApplyResponsePlugins(pluginsGlobals *PluginsGlobals, packet []byte, ttl *uint32) ([]byte, error) {
	if len(*pluginsGlobals.responsePlugins) == 0 && len(*pluginsGlobals.loggingPlugins) == 0 {
		return packet, nil
	}
	pluginsState.action = PluginsActionForward
	msg := dns.Msg{}
	if err := msg.Unpack(packet); err != nil {
		if len(packet) >= MinDNSPacketSize && HasTCFlag(packet) {
			err = nil
		}
		return packet, err
	}
	switch Rcode(packet) {
	case dns.RcodeSuccess:
		pluginsState.returnCode = PluginsReturnCodePass
	case dns.RcodeNameError:
		pluginsState.returnCode = PluginsReturnCodeNXDomain
	case dns.RcodeServerFailure:
		pluginsState.returnCode = PluginsReturnCodeServerError
	default:
		pluginsState.returnCode = PluginsReturnCodeResponseError
	}
	pluginsGlobals.RLock()
	defer pluginsGlobals.RUnlock()
	for _, plugin := range *pluginsGlobals.responsePlugins {
		if err := plugin.Eval(pluginsState, &msg); err != nil {
			pluginsState.action = PluginsActionDrop
			return packet, err
		}
		if pluginsState.action == PluginsActionReject {
			synth, err := RefusedResponseFromMessage(&msg, pluginsGlobals.refusedCodeInResponses, pluginsGlobals.respondWithIPv4, pluginsGlobals.respondWithIPv6, pluginsState.rejectTTL)
			if err != nil {
				return nil, err
			}
			dlog.Infof("Blocking [%s]", synth.Question[0].Name)
			pluginsState.synthResponse = synth
		}
		if pluginsState.action != PluginsActionForward {
			break
		}
	}
	if ttl != nil {
		setMaxTTL(&msg, *ttl)
	}
	packet2, err := msg.PackBuffer(packet)
	if err != nil {
		return packet, err
	}
	return packet2, nil
}

func (pluginsState *PluginsState) ApplyLoggingPlugins(pluginsGlobals *PluginsGlobals) error {
	if len(*pluginsGlobals.loggingPlugins) == 0 {
		return nil
	}
	pluginsState.requestEnd = time.Now()
	questionMsg := pluginsState.questionMsg
	if questionMsg == nil || len(questionMsg.Question) > 1 {
		return errors.New("Unexpected number of questions")
	}
	pluginsGlobals.RLock()
	defer pluginsGlobals.RUnlock()
	for _, plugin := range *pluginsGlobals.loggingPlugins {
		if err := plugin.Eval(pluginsState, questionMsg); err != nil {
			return err
		}
	}
	return nil
}