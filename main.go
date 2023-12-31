package main

import (
	"context"
	"encoding/json"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerV3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_manager "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/duration"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
	k8s_types "k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"strconv"
	"sync"
	"time"
)

func main() {
	logf.SetLogger(zap.New())

	wg := &sync.WaitGroup{}
	stop := make(chan struct{})
	defer close(stop)

	wg.Add(3)
	ctx, cancel := context.WithCancel(signals.SetupSignalHandler())
	mgr, err := createManager(ctx)
	if err != nil {
		panic(err)
	}
	go func() {
		defer wg.Done()
		err := mgr.Start(ctx)
		if err != nil {
			logf.Log.Error(err, "controller failed")
			cancel()
		}
	}()
	snapshotCache := cache.NewSnapshotCacheWithHeartbeating(ctx, true, cache.IDHash{}, nil, time.Second)
	go func() {
		defer wg.Done()
		err := runXDSServer(ctx, snapshotCache)
		if err != nil {
			logf.Log.Error(err, "xds service failed")
			cancel()
		}
	}()
	// goroutine
	go func() {
		ticker := time.NewTicker(time.Second * 5)
		defer func() {
			ticker.Stop()
			wg.Done()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				logf.Log.Info("tick", "time", t)
				configMap := &corev1.ConfigMap{}
				err := mgr.GetClient().Get(ctx, k8s_types.NamespacedName{Namespace: "default", Name: ConfigMapName}, configMap)
				if err != nil {
					logf.Log.Error(err, "Failed retrieving conf!")
				}
				all := []ServiceMeta{}
				err = json.Unmarshal([]byte(configMap.Data["config"]), &all)
				if err != nil {
					logf.Log.Error(err, "failed reading configMap")
				}
				var clusters []types.Resource
				var routes []*route.Route
				var listener []types.Resource
				for _, s := range all {
					// get the annotation from service
					var timeout *duration.Duration
					if timeoutAnnotation, ok := s.Annotation["mesh-timeout"]; ok {
						if duration, err := time.ParseDuration(timeoutAnnotation); err == nil {
							timeout = &durationpb.Duration{Seconds: int64(duration.Seconds())}
						}
					}
					if listenerPort, ok := s.Annotation["listener-port"]; ok {
						port, err := strconv.Atoi(listenerPort)
						if err != nil {
							logf.Log.Error(err, "invalid port value")
						} else {
							filterChain := &listenerV3.FilterChain{
								Name: "random-filter-chain",
								Filters: []*listenerV3.Filter{
									{
										Name: "envoy.filters.network.http_connection_manager",
										ConfigType: &listenerV3.Filter_TypedConfig{
											TypedConfig: marshalAny(&http_connection_manager.HttpConnectionManager{
												StatPrefix: "inbound",
											}),
										},
									},
								},
							}
							listener = append(listener, &listenerV3.Listener{
								Name: s.Name,
								Address: &corev3.Address{
									Address: &corev3.Address_SocketAddress{
										SocketAddress: &corev3.SocketAddress{
											Protocol: corev3.SocketAddress_TCP,
											Address:  "0.0.0.0",
											PortSpecifier: &corev3.SocketAddress_PortValue{
												PortValue: uint32(port),
											},
										},
									},
								},
								FilterChains: []*listenerV3.FilterChain{filterChain},
							})
						}
					}
					routes = append(routes, &route.Route{
						Name: s.Name,
						Match: &route.RouteMatch{
							Headers: []*route.HeaderMatcher{
								{
									Name: "service",
									HeaderMatchSpecifier: &route.HeaderMatcher_StringMatch{
										StringMatch: &matcherv3.StringMatcher{
											MatchPattern: &matcherv3.StringMatcher_Exact{
												Exact: s.Name,
											},
										},
									},
								},
							},
							PathSpecifier: &route.RouteMatch_Prefix{
								Prefix: "/",
							},
						},
						Action: &route.Route_Route{
							Route: &route.RouteAction{
								ClusterSpecifier: &route.RouteAction_Cluster{
									Cluster: s.Name,
								},
								Timeout: timeout,
							},
						},
					})
					clusters = append(clusters, &clusterv3.Cluster{
						Name: s.Name,
						LoadAssignment: &endpointv3.ClusterLoadAssignment{
							ClusterName: s.Name,
							Endpoints: []*endpointv3.LocalityLbEndpoints{
								{
									LbEndpoints: []*endpointv3.LbEndpoint{
										{
											HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
												Endpoint: &endpointv3.Endpoint{
													Address: &corev3.Address{
														Address: &corev3.Address_SocketAddress{
															SocketAddress: &corev3.SocketAddress{
																Address: s.Ip,
																PortSpecifier: &corev3.SocketAddress_PortValue{
																	PortValue: uint32(s.Port),
																},
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					})
				}

				snap, err := cache.NewSnapshot(time.Now().String(), map[resource.Type][]types.Resource{
					resource.ListenerType: listener,
					resource.ClusterType:  clusters,
					resource.EndpointType: {},
					resource.RouteType: {
						&route.RouteConfiguration{
							Name: "outbound_route",
							VirtualHosts: []*route.VirtualHost{{
								Name:    "mesh",
								Domains: []string{"*"},
								Routes:  routes,
							}},
						},
					},
				})
				if err != nil {
					logf.Log.Error(err, "failed creating snapshot")
				}
				for _, s := range all {
					err = snapshotCache.SetSnapshot(ctx, s.Name, snap)
				}
				if err != nil {
					logf.Log.Error(err, "failed setting snapshot")
				}
			}
		}
	}()

	<-ctx.Done()
	wg.Wait()
}

func marshalAny(pb proto.Message) *anypb.Any {
	any, err := ptypes.MarshalAny(pb)
	if err != nil {
		logf.Log.Error(err, "failed marshal")
	}
	return any
}
