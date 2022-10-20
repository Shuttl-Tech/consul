package peering

import (
	"context"
	"encoding/pem"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/sdk/testutil/retry"
	libassert "github.com/hashicorp/consul/test/integration/consul-container/libs/assert"
	libcluster "github.com/hashicorp/consul/test/integration/consul-container/libs/cluster"
	libnode "github.com/hashicorp/consul/test/integration/consul-container/libs/node"
	libservice "github.com/hashicorp/consul/test/integration/consul-container/libs/service"
	"github.com/hashicorp/consul/test/integration/consul-container/libs/utils"
)

const (
	acceptingPeerName = "accepting-to-dialer"
	dialingPeerName   = "dialing-to-acceptor"
)

// TestServer
// This test runs a few scenarios back to back
// 1. It makes sure that the peering stream send server address updates between peers.
//    It also verifies that dialing clusters will use this stored information to supersede the addresses
//    encoded in the peering token.
// 2. Rotate the CA in the exporting cluster and ensure services don't break
// 3. Terminate the server nodes in the exporting cluster and make sure the importing cluster can still dial it's
//    upstream.
//
// ## Steps
// ### Part 1
//  * Create an accepting cluster with 3 servers. 1 client should be used to host a service for export
//  * Create a single node dialing cluster.
//	* Create the peering and export the service. Verify it is working
//  * Incrementally replace the follower nodes.
// 	* Replace the leader node
//  * Verify the dialer can reach the new server nodes and the service becomes available.
// ### Part 2
//  * Push an update to the CA Configuration in the exporting cluster and wait for the new root to be generated
//  * Verify envoy client sidecar has two certificates for the upstream server
//  * Make sure there is still service connectivity from the importing cluster
// ### Part 3
//  * Terminate the server nodes in the exporting cluster
//  * Make sure there is still service connectivity from the importing cluster
func TestServer(t *testing.T) {
	var acceptingCluster, dialingCluster *libcluster.Cluster
	var acceptingClient, dialingClient *api.Client
	var clientSidecarService libservice.Service

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		acceptingCluster, acceptingClient = creatingAcceptingClusterAndSetup(t)
		wg.Done()
	}()
	defer func() {
		terminate(t, acceptingCluster)
	}()

	wg.Add(1)
	go func() {
		dialingCluster, dialingClient, clientSidecarService = createDialingClusterAndSetup(t)
		wg.Done()
	}()
	defer func() {
		terminate(t, dialingCluster)
	}()

	wg.Wait()

	generateReq := api.PeeringGenerateTokenRequest{
		PeerName: acceptingPeerName,
	}
	generateRes, _, err := acceptingClient.Peerings().GenerateToken(context.Background(), generateReq, &api.WriteOptions{})
	require.NoError(t, err)

	establishReq := api.PeeringEstablishRequest{
		PeerName:     dialingPeerName,
		PeeringToken: generateRes.PeeringToken,
	}
	_, _, err = dialingClient.Peerings().Establish(context.Background(), establishReq, &api.WriteOptions{})
	require.NoError(t, err)

	libassert.PeeringStatus(t, acceptingClient, acceptingPeerName, api.PeeringStateActive)
	libassert.PeeringExports(t, acceptingClient, acceptingPeerName, 1)

	_, port := clientSidecarService.GetAddr()
	libassert.HTTPServiceEchoes(t, "localhost", port)

	t.Run("test rotating servers", func(t *testing.T) {

		// Start by replacing the Followers
		leader, err := acceptingCluster.Leader()
		require.NoError(t, err)

		followers, err := acceptingCluster.Followers()
		require.NoError(t, err)
		require.Len(t, followers, 2)

		for idx, follower := range followers {
			t.Log("Removing follower", idx)
			rotateServer(t, acceptingCluster, acceptingClient, follower)
		}

		t.Log("Removing leader")
		rotateServer(t, acceptingCluster, acceptingClient, leader)

		libassert.PeeringStatus(t, acceptingClient, acceptingPeerName, api.PeeringStateActive)
		libassert.PeeringExports(t, acceptingClient, acceptingPeerName, 1)

		libassert.HTTPServiceEchoes(t, "localhost", port)
	})

	t.Run("rotate exporting cluster's root CA", func(t *testing.T) {
		config, meta, err := acceptingClient.Connect().CAGetConfig(&api.QueryOptions{})
		require.NoError(t, err)

		t.Logf("%+v", config)

		req := &api.CAConfig{
			Provider: "consul",
			Config: map[string]interface{}{
				"PrivateKeyType": "ec",
				"PrivateKeyBits": 384,
			},
		}
		_, err = acceptingClient.Connect().CASetConfig(req, &api.WriteOptions{})
		require.NoError(t, err)

		// wait up to 30 seconds for the update
		_, _, err = acceptingClient.Connect().CAGetConfig(&api.QueryOptions{
			WaitIndex: meta.LastIndex,
			WaitTime:  30 * time.Second,
		})
		require.NoError(t, err)

		// There should be two root certs now
		rootList, _, err := acceptingClient.Connect().CARoots(&api.QueryOptions{})
		require.NoError(t, err)
		require.Len(t, rootList.Roots, 2)

		// Connectivity should still be contained
		_, port := clientSidecarService.GetAddr()
		libassert.HTTPServiceEchoes(t, "localhost", port)

		verifySidecarHasTwoRootCAs(t, clientSidecarService)

	})

	t.Run("terminate exporting clusters servers and ensure imported services are still reachable", func(t *testing.T) {
		// Keep this list for later
		newNodes, err := acceptingCluster.Clients()
		require.NoError(t, err)

		serverNodes, err := acceptingCluster.Servers()
		require.NoError(t, err)
		for _, node := range serverNodes {
			require.NoError(t, node.Terminate())
		}

		// Remove the nodes from the cluster to prevent double-termination
		acceptingCluster.Nodes = newNodes

		// ensure any transitory actions like replication cleanup would not affect the next verifications
		time.Sleep(30 * time.Second)

		_, port := clientSidecarService.GetAddr()
		libassert.HTTPServiceEchoes(t, "localhost", port)
	})
}

func terminate(t *testing.T, cluster *libcluster.Cluster) {
	err := cluster.Terminate()
	require.NoError(t, err)
}

// creatingAcceptingClusterAndSetup creates a cluster with 3 servers and 1 client.
// It also creates and registers a service+sidecar.
// The API client returned is pointed at the client agent.
func creatingAcceptingClusterAndSetup(t *testing.T) (*libcluster.Cluster, *api.Client) {
	var configs []libnode.Config

	numServer := 3
	for i := 0; i < numServer; i++ {
		configs = append(configs,
			libnode.Config{
				HCL: `node_name="` + utils.RandName("consul-server") + `"
					ports {
					  dns = 8600
					  http = 8500
					  https = 8501
					  grpc = 8502
    				  grpc_tls = 8503
					  serf_lan = 8301
					  serf_wan = 8302
					  server = 8300
					}	
					bind_addr = "0.0.0.0"
					advertise_addr = "{{ GetInterfaceIP \"eth0\" }}"
					log_level="DEBUG"
					server=true
					peering {
						enabled=true
					}
					bootstrap_expect=3
					connect {
					  enabled = true
					}`,
				Cmd:     []string{"agent", "-client=0.0.0.0"},
				Version: *utils.TargetVersion,
				Image:   *utils.TargetImage,
			},
		)
	}

	// Add a stable client to register the service
	configs = append(configs,
		libnode.Config{
			HCL: `node_name="` + utils.RandName("consul-client") + `"
					ports {
					  dns = 8600
					  http = 8500
					  https = 8501
					  grpc = 8502
    				  grpc_tls = 8503
					  serf_lan = 8301
					  serf_wan = 8302
					}	
					bind_addr = "0.0.0.0"
					advertise_addr = "{{ GetInterfaceIP \"eth0\" }}"
					log_level="DEBUG"
					peering {
						enabled=true
					}
					connect {
					  enabled = true
					}`,
			Cmd:     []string{"agent", "-client=0.0.0.0"},
			Version: *utils.TargetVersion,
			Image:   *utils.TargetImage,
		},
	)

	cluster, err := libcluster.New(configs)
	require.NoError(t, err)

	// Use the client agent as the HTTP endpoint since we will not rotate it
	clientNode := cluster.Nodes[3]
	client := clientNode.GetClient()
	libcluster.WaitForLeader(t, cluster, client)
	libcluster.WaitForMembers(t, client, 4)

	// Default Proxy Settings
	ok, err := utils.ApplyDefaultProxySettings(client)
	require.NoError(t, err)
	require.True(t, ok)

	// Create the mesh gateway for dataplane traffic
	_, err = libservice.NewGatewayService(context.Background(), "mesh", "mesh", clientNode)
	require.NoError(t, err)

	// Create a service and proxy instance
	_, _, err = libservice.CreateAndRegisterStaticServerAndSidecar(clientNode)
	require.NoError(t, err)

	libassert.CatalogServiceExists(t, client, "static-server")
	libassert.CatalogServiceExists(t, client, "static-server-sidecar-proxy")

	// Export the service
	config := &api.ExportedServicesConfigEntry{
		Name: "default",
		Services: []api.ExportedService{
			{
				Name: "static-server",
				Consumers: []api.ServiceConsumer{
					{Peer: acceptingPeerName},
				},
			},
		},
	}
	ok, _, err = client.ConfigEntries().Set(config, &api.WriteOptions{})
	require.NoError(t, err)
	require.True(t, ok)

	return cluster, client
}

// createDialingClusterAndSetup creates a cluster for peering with a single dev agent
func createDialingClusterAndSetup(t *testing.T) (*libcluster.Cluster, *api.Client, libservice.Service) {
	configs := []libnode.Config{
		{
			HCL: `ports {
  dns = 8600
  http = 8500
  https = 8501
  grpc = 8502
  grpc_tls = 8503
  serf_lan = 8301
  serf_wan = 8302
  server = 8300
}
bind_addr = "0.0.0.0"
advertise_addr = "{{ GetInterfaceIP \"eth0\" }}"
log_level="DEBUG"
server=true
bootstrap = true
peering {
	enabled=true
}
datacenter = "dc2"
connect {
  enabled = true
}`,
			Cmd:     []string{"agent", "-client=0.0.0.0"},
			Version: *utils.TargetVersion,
			Image:   *utils.TargetImage,
		},
	}

	cluster, err := libcluster.New(configs)
	require.NoError(t, err)

	node := cluster.Nodes[0]
	client := node.GetClient()
	libcluster.WaitForLeader(t, cluster, client)
	libcluster.WaitForMembers(t, client, 1)

	// Default Proxy Settings
	ok, err := utils.ApplyDefaultProxySettings(client)
	require.NoError(t, err)
	require.True(t, ok)

	// Create the mesh gateway for dataplane traffic
	_, err = libservice.NewGatewayService(context.Background(), "mesh", "mesh", node)
	require.NoError(t, err)

	// Create a service and proxy instance
	clientProxyService, err := libservice.CreateAndRegisterStaticClientSidecar(node, dialingPeerName, true)
	require.NoError(t, err)

	libassert.CatalogServiceExists(t, client, "static-client-sidecar-proxy")

	return cluster, client, clientProxyService
}

// rotateServer add a new server node to the cluster, then forces the prior node to leave.
func rotateServer(t *testing.T, cluster *libcluster.Cluster, client *api.Client, node libnode.Node) {
	config := libnode.Config{
		HCL: `encrypt="` + cluster.EncryptKey + `"
			node_name="` + utils.RandName("consul-server") + `"
			ports {
			  dns = 8600
			  http = 8500
			  https = 8501
			  grpc = 8502
			  grpc_tls = 8503
			  serf_lan = 8301
			  serf_wan = 8302
			  server = 8300
			}	
			bind_addr = "0.0.0.0"
			advertise_addr = "{{ GetInterfaceIP \"eth0\" }}"
			log_level="DEBUG"
			server=true
			peering {
				enabled=true
			}
			connect {
			  enabled = true
			}`,
		Cmd:     []string{"agent", "-client=0.0.0.0"},
		Version: *utils.TargetVersion,
		Image:   *utils.TargetImage,
	}

	c, err := libnode.NewConsulContainer(context.Background(), config)
	require.NoError(t, err, "could not start new node")

	require.NoError(t, cluster.AddNodes([]libnode.Node{c}))

	libcluster.WaitForMembers(t, client, 5)

	require.NoError(t, cluster.RemoveNode(node))

	libcluster.WaitForMembers(t, client, 4)
}

func verifySidecarHasTwoRootCAs(t *testing.T, sidecar libservice.Service) {
	connectContainer, ok := sidecar.(*libservice.ConnectContainer)
	require.True(t, ok)
	_, adminPort := connectContainer.GetAdminAddr()

	failer := func() *retry.Timer {
		return &retry.Timer{Timeout: 30 * time.Second, Wait: 1 * time.Second}
	}

	retry.RunWith(failer(), t, func(r *retry.R) {
		dump, err := libservice.GetEnvoyConfigDump(adminPort)
		if err != nil {
			r.Fatal("could not curl envoy configuration")
		}

		// Make sure there are two certs in the sidecar
		filter := `.configs[] | select(.["@type"] | contains("type.googleapis.com/envoy.admin.v3.ClustersConfigDump")).dynamic_active_clusters[] | select(.cluster.name | contains("static-server.default.dialing-to-acceptor.external")).cluster.transport_socket.typed_config.common_tls_context.validation_context.trusted_ca.inline_string`
		results, err := utils.JQFilter(dump, filter)
		if err != nil {
			r.Fatal("could not parse envoy configuration")
		}
		if len(results) != 1 {
			r.Fatal("could not find certificates in cluster TLS context")
		}

		rest := []byte(results[0])
		var count int
		for len(rest) > 0 {
			var p *pem.Block
			p, rest = pem.Decode(rest)
			if p == nil {
				break
			}
			count++
		}

		if count != 2 {
			r.Fatalf("expected 2 TLS certificates and %d present", count)
		}
	})
}