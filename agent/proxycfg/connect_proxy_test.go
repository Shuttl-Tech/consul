package proxycfg

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseProxyConfig(t *testing.T) {
	tests := []struct {
		name    string
		input   map[string]interface{}
		want    hcpMetricsConfig
		wantErr string
	}{
		{
			name: "hcp metrics port set, int",
			input: map[string]interface{}{
				"envoy_hcp_metrics_bind_port": 3000,
			},
			want: hcpMetricsConfig{
				HCPMetricsBindPort: 3000,
			},
		},
		{
			name: "hcp metrics port defaults to 0, int",
			input: map[string]interface{}{
				"envoy_hcp_metrics_bind_port": 80000,
			},
			want: hcpMetricsConfig{
				HCPMetricsBindPort: 0,
			},
			wantErr: "invalid envoy_hcp_metrics_bind_port: 80000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHCPMetricsConfig(tt.input)

			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.want, got)
		})
	}
}
