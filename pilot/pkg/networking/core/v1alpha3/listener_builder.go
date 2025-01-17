// Copyright 2019 Istio Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha3

import (
	xdsapi "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	tcp_proxy "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/tcp_proxy/v2"
	xdsutil "github.com/envoyproxy/go-control-plane/pkg/util"
	gogoproto "github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/core/v1alpha3/envoyfilter"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pkg/proto"
	"istio.io/pkg/log"
)

var (
	// Precompute these filters as an optimization
	blackholeAnyMarshalling    = newBlackholeFilter(true)
	blackholeStructMarshalling = newBlackholeFilter(false)
)

// A stateful listener builder
// Support the below intentions
// 1. Use separate inbound capture listener(:15006) and outbound capture listener(:15001)
// 2. The above listeners use bind_to_port sub listeners or filter chains.
type ListenerBuilder struct {
	node                   *model.Proxy
	gatewayListeners       []*xdsapi.Listener
	inboundListeners       []*xdsapi.Listener
	outboundListeners      []*xdsapi.Listener
	virtualListener        *xdsapi.Listener
	virtualInboundListener *xdsapi.Listener
	useInboundFilterChain  bool
}

func insertOriginalListenerName(chain *listener.FilterChain, listenerName string) {
	if chain.Metadata == nil {
		chain.Metadata = &core.Metadata{
			FilterMetadata: map[string]*types.Struct{},
		}
	}
	if chain.Metadata.FilterMetadata[PilotMetaKey] == nil {
		chain.Metadata.FilterMetadata[PilotMetaKey] = &types.Struct{
			Fields: map[string]*types.Value{},
		}
	}
	chain.Metadata.FilterMetadata[PilotMetaKey].Fields["original_listener_name"] =
		&types.Value{Kind: &types.Value_StringValue{StringValue: listenerName}}
}

// Setup the filter chain match so that the match should work under both
// - bind_to_port == false listener
// - virtual inbound listener
func amendFilterChainMatchFromInboundListener(chain *listener.FilterChain, l *xdsapi.Listener, needTLS bool) (*listener.FilterChain, bool) {
	if chain.FilterChainMatch == nil {
		chain.FilterChainMatch = &listener.FilterChainMatch{}
	}
	listenerAddress := l.Address
	if sockAddr := listenerAddress.GetSocketAddress(); sockAddr != nil {
		chain.FilterChainMatch.DestinationPort = &types.UInt32Value{Value: sockAddr.GetPortValue()}
		if cidr := util.ConvertAddressToCidr(sockAddr.GetAddress()); cidr != nil {
			if chain.FilterChainMatch.PrefixRanges != nil && len(chain.FilterChainMatch.PrefixRanges) != 1 {
				log.Debugf("Intercepted inbound listener %s have neither 0 or 1 prefix ranges. Actual:  %d",
					l.Name, len(chain.FilterChainMatch.PrefixRanges))
			}
			chain.FilterChainMatch.PrefixRanges = []*core.CidrRange{util.ConvertAddressToCidr(sockAddr.GetAddress())}
		}
		insertOriginalListenerName(chain, l.Name)
	}
	for _, filter := range l.ListenerFilters {
		if needTLS = needTLS || filter.Name == xdsutil.TlsInspector; needTLS {
			break
		}
	}
	return chain, needTLS
}

// Accumulate the filter chains from per proxy service listeners
func reduceInboundListenerToFilterChains(listeners []*xdsapi.Listener) ([]*listener.FilterChain, bool) {
	needTLS := false
	chains := make([]*listener.FilterChain, 0)
	for _, l := range listeners {
		// default bindToPort is true and these listener should be skipped
		if v1Opt := l.GetDeprecatedV1(); v1Opt == nil || v1Opt.BindToPort == nil || v1Opt.BindToPort.Value {
			// A listener on real port should not be intercepted by virtual inbound listener
			continue
		}
		for _, c := range l.FilterChains {
			newChain, needTLSLocal := amendFilterChainMatchFromInboundListener(gogoproto.Clone(c).(*listener.FilterChain), l, needTLS)
			chains = append(chains, newChain)
			needTLS = needTLS || needTLSLocal
		}
	}
	return chains, needTLS
}

func (builder *ListenerBuilder) aggregateVirtualInboundListener() *ListenerBuilder {
	// Deprecated by envoyproxy. Replaced
	// 1. filter chains in this listener
	// 2. explicit original_dst listener filter
	// UseOriginalDst: proto.BoolTrue,
	builder.virtualInboundListener.UseOriginalDst = nil
	builder.virtualInboundListener.ListenerFilters = append(builder.virtualInboundListener.ListenerFilters,
		&listener.ListenerFilter{
			Name: xdsutil.OriginalDestination,
		},
	)
	// TODO: Trim the inboundListeners properly. Those that have been added to filter chains should
	// be removed while those that haven't been added need to remain in the inboundListeners list.
	filterChains, needTLS := reduceInboundListenerToFilterChains(builder.inboundListeners)

	builder.virtualInboundListener.FilterChains =
		append(builder.virtualInboundListener.FilterChains, filterChains...)

	if needTLS {
		builder.virtualInboundListener.ListenerFilters =
			append(builder.virtualInboundListener.ListenerFilters, &listener.ListenerFilter{
				Name: xdsutil.TlsInspector,
			})
	}
	return builder
}

func NewListenerBuilder(node *model.Proxy) *ListenerBuilder {
	builder := &ListenerBuilder{
		node: node,
		// The extra inbound listener has no side effect for iptables that doesn't redirect to 15006
		useInboundFilterChain: true,
	}
	return builder
}

func (builder *ListenerBuilder) buildSidecarInboundListeners(configgen *ConfigGeneratorImpl,
	env *model.Environment, node *model.Proxy, push *model.PushContext) *ListenerBuilder {
	builder.inboundListeners = configgen.buildSidecarInboundListeners(env, node, push)
	return builder
}

func (builder *ListenerBuilder) buildSidecarOutboundListeners(configgen *ConfigGeneratorImpl,
	env *model.Environment, node *model.Proxy, push *model.PushContext) *ListenerBuilder {
	builder.outboundListeners = configgen.buildSidecarOutboundListeners(env, node, push)
	return builder
}

func (builder *ListenerBuilder) buildManagementListeners(_ *ConfigGeneratorImpl,
	env *model.Environment, node *model.Proxy, _ *model.PushContext) *ListenerBuilder {

	noneMode := node.GetInterceptionMode() == model.InterceptionNone

	// Do not generate any management port listeners if the user has specified a SidecarScope object
	// with ingress listeners. Specifying the ingress listener implies that the user wants
	// to only have those specific listeners and nothing else, in the inbound path.
	if node.SidecarScope.HasCustomIngressListeners || noneMode {
		return builder
	}
	// Let ServiceDiscovery decide which IP and Port are used for management if
	// there are multiple IPs
	mgmtListeners := make([]*xdsapi.Listener, 0)
	for _, ip := range node.IPAddresses {
		managementPorts := env.ManagementPorts(ip)
		management := buildSidecarInboundMgmtListeners(node, env, managementPorts, ip)
		mgmtListeners = append(mgmtListeners, management...)
	}
	addresses := make(map[string]*xdsapi.Listener)
	for _, listener := range builder.inboundListeners {
		if listener != nil {
			addresses[listener.Address.String()] = listener
		}
	}
	for _, listener := range builder.outboundListeners {
		if listener != nil {
			addresses[listener.Address.String()] = listener
		}
	}

	// If management listener port and service port are same, bad things happen
	// when running in kubernetes, as the probes stop responding. So, append
	// non overlapping listeners only.
	for i := range mgmtListeners {
		m := mgmtListeners[i]
		addressString := m.Address.String()
		existingListener, ok := addresses[addressString]
		if ok {
			log.Warnf("Omitting listener for management address %s due to collision with service listener (%s)",
				m.Name, existingListener.Name)
			continue
		} else {
			// dedup management listeners as well
			addresses[addressString] = m
			builder.inboundListeners = append(builder.inboundListeners, m)
		}

	}
	return builder
}

func (builder *ListenerBuilder) buildVirtualOutboundListener(
	configgen *ConfigGeneratorImpl,
	env *model.Environment, node *model.Proxy, push *model.PushContext) *ListenerBuilder {

	var isTransparentProxy *types.BoolValue
	if node.GetInterceptionMode() == model.InterceptionTproxy {
		isTransparentProxy = proto.BoolTrue
	}

	tcpProxyFilter := newTCPProxyOutboundListenerFilter(env, node)

	filterChains := []*listener.FilterChain{
		{
			Filters: []*listener.Filter{tcpProxyFilter},
		},
	}

	// The virtual listener will handle all traffic that does not match any other listeners, and will
	// blackhole/passthrough depending on the outbound traffic policy. When passthrough is enabled,
	// this has the risk of triggering infinite loops when requests are sent to the pod's IP, as it will
	// send requests to itself. To block this we add an additional filter chain before that will always blackhole.
	if features.RestrictPodIPTrafficLoops.Get() {
		var cidrRanges []*core.CidrRange
		for _, ip := range node.IPAddresses {
			cidrRanges = append(cidrRanges, util.ConvertAddressToCidr(ip))
		}
		blackhole := &blackholeStructMarshalling
		if util.IsXDSMarshalingToAnyEnabled(node) {
			blackhole = &blackholeAnyMarshalling
		}
		filterChains = append([]*listener.FilterChain{{
			FilterChainMatch: &listener.FilterChainMatch{
				PrefixRanges: cidrRanges,
			},
			Filters: []*listener.Filter{blackhole},
		}}, filterChains...)
	}

	actualWildcard, _ := getActualWildcardAndLocalHost(node)

	// add an extra listener that binds to the port that is the recipient of the iptables redirect
	ipTablesListener := &xdsapi.Listener{
		Name:           VirtualOutboundListenerName,
		Address:        util.BuildAddress(actualWildcard, uint32(env.Mesh.ProxyListenPort)),
		Transparent:    isTransparentProxy,
		UseOriginalDst: proto.BoolTrue,
		FilterChains:   filterChains,
	}
	configgen.onVirtualOutboundListener(env, node, push, ipTablesListener)
	builder.virtualListener = ipTablesListener
	return builder
}

// TProxy uses only the virtual outbound listener on 15001 for both directions
// but we still ship the no-op virtual inbound listener, so that the code flow is same across REDIRECT and TPROXY.
func (builder *ListenerBuilder) buildVirtualInboundListener(env *model.Environment, node *model.Proxy) *ListenerBuilder {
	var isTransparentProxy *types.BoolValue
	if node.GetInterceptionMode() == model.InterceptionTproxy {
		isTransparentProxy = proto.BoolTrue
	}

	actualWildcard, _ := getActualWildcardAndLocalHost(node)
	// add an extra listener that binds to the port that is the recipient of the iptables redirect
	builder.virtualInboundListener = &xdsapi.Listener{
		Name:           VirtualInboundListenerName,
		Address:        util.BuildAddress(actualWildcard, ProxyInboundListenPort),
		Transparent:    isTransparentProxy,
		UseOriginalDst: proto.BoolTrue,
		FilterChains:   newInboundPassthroughFilterChains(env, node),
	}
	if builder.useInboundFilterChain {
		builder.aggregateVirtualInboundListener()
	}
	return builder
}

func (builder *ListenerBuilder) patchListeners(push *model.PushContext) {
	if builder.node.Type == model.Router {
		envoyfilter.ApplyListenerPatches(networking.EnvoyFilter_GATEWAY, builder.node, push, builder.gatewayListeners, false)
		return
	}

	patchOneListener := func(listener *xdsapi.Listener, ctx networking.EnvoyFilter_PatchContext) *xdsapi.Listener {
		if listener == nil {
			return nil
		}
		tempArray := []*xdsapi.Listener{listener}
		tempArray = envoyfilter.ApplyListenerPatches(ctx, builder.node, push, tempArray, true)
		// temp array will either be empty [if virtual listener was removed] or will have a modified listener
		if len(tempArray) == 0 {
			return nil
		}
		return tempArray[0]
	}
	builder.virtualListener = patchOneListener(builder.virtualListener, networking.EnvoyFilter_SIDECAR_OUTBOUND)
	builder.virtualInboundListener = patchOneListener(builder.virtualInboundListener, networking.EnvoyFilter_SIDECAR_INBOUND)
	builder.inboundListeners = envoyfilter.ApplyListenerPatches(networking.EnvoyFilter_SIDECAR_INBOUND, builder.node,
		push, builder.inboundListeners, false)
	builder.outboundListeners = envoyfilter.ApplyListenerPatches(networking.EnvoyFilter_SIDECAR_OUTBOUND, builder.node,
		push, builder.outboundListeners, false)
}

func (builder *ListenerBuilder) getListeners() []*xdsapi.Listener {
	if builder.node.Type == model.SidecarProxy {
		nInbound, nOutbound := len(builder.inboundListeners), len(builder.outboundListeners)
		nVirtual, nVirtualInbound := 0, 0
		if builder.virtualListener != nil {
			nVirtual = 1
		}
		if builder.virtualInboundListener != nil {
			nVirtualInbound = 1
		}
		nListener := nInbound + nOutbound + nVirtual + nVirtualInbound

		listeners := make([]*xdsapi.Listener, 0, nListener)
		listeners = append(listeners, builder.inboundListeners...)
		listeners = append(listeners, builder.outboundListeners...)
		if builder.virtualListener != nil {
			listeners = append(listeners, builder.virtualListener)
		}
		if builder.virtualInboundListener != nil {
			listeners = append(listeners, builder.virtualInboundListener)
		}

		log.Debugf("Build %d listeners for node %s including %d inbound, %d outbound, %d virtual and %d virtual inbound listeners",
			nListener,
			builder.node.ID,
			nInbound, nOutbound,
			nVirtual, nVirtualInbound)
		return listeners
	}

	return builder.gatewayListeners
}

// Creates a new filter that will always send traffic to the blackhole cluster
func newBlackholeFilter(enableAny bool) listener.Filter {
	tcpProxy := &tcp_proxy.TcpProxy{
		StatPrefix:       util.BlackHoleCluster,
		ClusterSpecifier: &tcp_proxy.TcpProxy_Cluster{Cluster: util.BlackHoleCluster},
	}

	filter := listener.Filter{
		Name: xdsutil.TCPProxy,
	}

	if enableAny {
		filter.ConfigType = &listener.Filter_TypedConfig{TypedConfig: util.MessageToAny(tcpProxy)}
	} else {
		filter.ConfigType = &listener.Filter_Config{Config: util.MessageToStruct(tcpProxy)}
	}
	return filter
}

// Create pass through filter chains matching ipv4 address and ipv6 address independently.
func newInboundPassthroughFilterChains(env *model.Environment, node *model.Proxy) []*listener.FilterChain {
	// ipv4 and ipv6
	filterChains := make([]*listener.FilterChain, 0, 2)
	for _, clusterName := range []string{util.InboundPassthroughClusterIpv4, util.InboundPassthroughClusterIpv6} {

		tcpProxy := &tcp_proxy.TcpProxy{
			StatPrefix:       clusterName,
			ClusterSpecifier: &tcp_proxy.TcpProxy_Cluster{Cluster: clusterName},
		}

		matchingIP := ""
		if clusterName == util.InboundPassthroughClusterIpv4 {
			matchingIP = "0.0.0.0/0"
		} else if clusterName == util.InboundPassthroughClusterIpv6 {
			matchingIP = "::0/0"
		}

		filterChainMatch := listener.FilterChainMatch{
			// Port : EMPTY to match all ports
			PrefixRanges: []*core.CidrRange{
				util.ConvertAddressToCidr(matchingIP),
			},
		}
		setAccessLog(env, node, tcpProxy)
		filter := &listener.Filter{
			Name: xdsutil.TCPProxy,
		}

		if util.IsXDSMarshalingToAnyEnabled(node) {
			filter.ConfigType = &listener.Filter_TypedConfig{TypedConfig: util.MessageToAny(tcpProxy)}
		} else {
			filter.ConfigType = &listener.Filter_Config{Config: util.MessageToStruct(tcpProxy)}
		}
		filterChain := &listener.FilterChain{
			FilterChainMatch: &filterChainMatch,
			Filters: []*listener.Filter{
				filter,
			},
		}
		insertOriginalListenerName(filterChain, VirtualInboundListenerName)
		filterChains = append(filterChains, filterChain)
	}

	return filterChains
}

func newTCPProxyOutboundListenerFilter(env *model.Environment, node *model.Proxy) *listener.Filter {
	tcpProxy := &tcp_proxy.TcpProxy{
		StatPrefix:       util.BlackHoleCluster,
		ClusterSpecifier: &tcp_proxy.TcpProxy_Cluster{Cluster: util.BlackHoleCluster},
	}
	if isAllowAnyOutbound(node) {
		// We need a passthrough filter to fill in the filter stack for orig_dst listener
		tcpProxy = &tcp_proxy.TcpProxy{
			StatPrefix:       util.PassthroughCluster,
			ClusterSpecifier: &tcp_proxy.TcpProxy_Cluster{Cluster: util.PassthroughCluster},
		}
		setAccessLog(env, node, tcpProxy)
	}

	filter := listener.Filter{
		Name: xdsutil.TCPProxy,
	}

	if util.IsXDSMarshalingToAnyEnabled(node) {
		filter.ConfigType = &listener.Filter_TypedConfig{TypedConfig: util.MessageToAny(tcpProxy)}
	} else {
		filter.ConfigType = &listener.Filter_Config{Config: util.MessageToStruct(tcpProxy)}
	}
	return &filter
}

func isAllowAnyOutbound(node *model.Proxy) bool {
	return node.SidecarScope.OutboundTrafficPolicy != nil && node.SidecarScope.OutboundTrafficPolicy.Mode == networking.OutboundTrafficPolicy_ALLOW_ANY
}
