package extensioncommon

import (
	"testing"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/require"
)

func makeTestRuntimeConfig() RuntimeConfig {
	sn := api.CompoundServiceName{Name: "api"}

	rc := RuntimeConfig{
		Kind:        api.ServiceKindConnectProxy,
		ServiceName: sn,
		Upstreams: map[api.CompoundServiceName]*UpstreamData{
			sn: {
				EnvoyID:           "eid",
				OutgoingProxyKind: api.ServiceKindTerminatingGateway,
				SNI: map[string]struct{}{
					"sni1": {},
					"sni2": {},
				},
			},
		},
	}
	return rc
}

func TestRuntimeConfig_IsUpstream(t *testing.T) {
	rc := makeTestRuntimeConfig()
	require.True(t, rc.IsUpstream())
	delete(rc.Upstreams, rc.ServiceName)
	require.False(t, rc.IsUpstream())
}

func TestRuntimeConfig_MatchesUpstreamServiceSNI(t *testing.T) {
	rc := makeTestRuntimeConfig()
	require.True(t, rc.MatchesUpstreamServiceSNI("sni1"))
	require.True(t, rc.MatchesUpstreamServiceSNI("sni2"))
	require.False(t, rc.MatchesUpstreamServiceSNI("sni3"))
}

func TestRuntimeConfig_EnvoyID(t *testing.T) {
	rc := makeTestRuntimeConfig()
	require.Equal(t, "eid", rc.EnvoyID())
}

func TestRuntimeConfig_OutgoingProxyKind(t *testing.T) {
	rc := makeTestRuntimeConfig()
	require.Equal(t, api.ServiceKindTerminatingGateway, rc.OutgoingProxyKind())
}
