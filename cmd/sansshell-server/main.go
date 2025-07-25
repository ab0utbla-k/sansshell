//go:build go1.17
// +build go1.17

/* Copyright (c) 2019 Snowflake Inc. All rights reserved.

   Licensed under the Apache License, Version 2.0 (the
   "License"); you may not use this file except in compliance
   with the License.  You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing,
   software distributed under the License is distributed on an
   "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
   KIND, either express or implied.  See the License for the
   specific language governing permissions and limitations
   under the License.
*/

// Package main implements the SansShell server.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"
	"go.opentelemetry.io/otel"
	prometheus_exporter "go.opentelemetry.io/otel/exporters/prometheus"
	otelmetricsdk "go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/grpc"
	channelz "google.golang.org/grpc/channelz/service"
	"google.golang.org/grpc/reflection"

	_ "gocloud.dev/blob/azureblob" // Pull in Azure blob support
	_ "gocloud.dev/blob/fileblob"  // Pull in file blob support
	_ "gocloud.dev/blob/gcsblob"   // Pull in GCS blob support
	_ "gocloud.dev/blob/s3blob"    // Pull in S3 blob support

	"github.com/Snowflake-Labs/sansshell/auth/mtls"
	mtlsFlags "github.com/Snowflake-Labs/sansshell/auth/mtls/flags"
	"github.com/Snowflake-Labs/sansshell/auth/opa"
	"github.com/Snowflake-Labs/sansshell/auth/rpcauth"
	"github.com/Snowflake-Labs/sansshell/cmd/sansshell-server/server"
	"github.com/Snowflake-Labs/sansshell/cmd/util"
	"github.com/Snowflake-Labs/sansshell/services"
	ssutil "github.com/Snowflake-Labs/sansshell/services/util"
	"github.com/Snowflake-Labs/sansshell/telemetry/metrics"

	// Import the server modules you want to expose, they automatically register

	// Ansible needs a real import to bind flags.
	ansible "github.com/Snowflake-Labs/sansshell/services/ansible/server"
	_ "github.com/Snowflake-Labs/sansshell/services/dns/server"
	_ "github.com/Snowflake-Labs/sansshell/services/exec/server"
	_ "github.com/Snowflake-Labs/sansshell/services/httpoverrpc/server"
	_ "github.com/Snowflake-Labs/sansshell/services/tlsinfo/server"

	fdbserver "github.com/Snowflake-Labs/sansshell/services/fdb/server"
	_ "github.com/Snowflake-Labs/sansshell/services/healthcheck/server"
	_ "github.com/Snowflake-Labs/sansshell/services/localfile/server"
	mpa "github.com/Snowflake-Labs/sansshell/services/mpa/server"
	_ "github.com/Snowflake-Labs/sansshell/services/network/server"
	_ "github.com/Snowflake-Labs/sansshell/services/power/server"

	// Packages needs a real import to bind flags.
	packages "github.com/Snowflake-Labs/sansshell/services/packages/server"
	// Process needs a real import to bind flags.
	process "github.com/Snowflake-Labs/sansshell/services/process/server"
	// Sansshell server needs a real import to get at Version
	ssserver "github.com/Snowflake-Labs/sansshell/services/sansshell/server"
	_ "github.com/Snowflake-Labs/sansshell/services/service/server"
	_ "github.com/Snowflake-Labs/sansshell/services/sysinfo/server"
)

var (
	//go:embed default-policy.rego
	defaultPolicy string

	policyFlag    = flag.String("policy", defaultPolicy, "Local OPA policy governing access.  If empty, use builtin policy.")
	policyFile    = flag.String("policy-file", "", "Path to a file with an OPA policy.  If empty, uses --policy.")
	apiVersion    = flag.String("api-version", "1.0.0", "Version of the Sansshell services API accepted by the server. Policy set in proxy and server must be verified before upgrading against API extensions to avoid unintentional side effects.")
	hostport      = flag.String("hostport", "localhost:50042", "Where to listen for connections.")
	debugport     = flag.String("debugport", "localhost:50044", "A separate port for http debug pages. Set to an empty string to disable.")
	metricsport   = flag.String("metricsport", "localhost:50047", "A separate port for http debug pages. Set to an empty string to disable.")
	unixSocket    = flag.String("unix-socket", "", "Path to a Unix socket to listen on in addition to hostport. The socket supports plaintext (non-TLS) communication. Set to an empty string to disable.")
	credSource    = flag.String("credential-source", mtlsFlags.Name(), fmt.Sprintf("Method used to obtain mTLS credentials (one of [%s])", strings.Join(mtls.Loaders(), ",")))
	verbosity     = flag.Int("v", 0, "Verbosity level. > 0 indicates more extensive logging")
	validate      = flag.Bool("validate", false, "If true will evaluate the policy and then exit (non-zero on error)")
	justification = flag.Bool("justification", false, "If true then justification (which is logged and possibly validated) must be passed along in the client context Metadata with the key '"+rpcauth.ReqJustKey+"'")
	version       bool

	fdbCLIEnvList ssutil.StringSliceFlag
)

func init() {
	flag.StringVar(&fdbserver.FDBServer, "fdbserver", "/usr/sbin/fdbserver", "Path to fdbserver binary.")
	flag.StringVar(&fdbserver.FDBCLI, "fdbcli", "/usr/bin/fdbcli", "Path to fdbcli binary. API assumes version 7.1. Older versions may not implement some commands.")
	flag.StringVar(&fdbserver.FDBCLIUser, "fdbcli-user", "", "User to change to when running fdbcli")
	flag.StringVar(&fdbserver.FDBCLIGroup, "fdbcli-group", "", "Group to change to when running fdbcli")
	flag.StringVar(&fdbserver.FDBCLIEnvFile, "fdbcli-env-file", "/etc/fdb.env", "File with environmental variables to add to the environment for fdbcli invocations, ignored if nonexistent")
	fdbCLIEnvList.Target = &fdbserver.FDBCLIEnvList
	*fdbCLIEnvList.Target = append(*fdbCLIEnvList.Target, "") // To set a default
	flag.Var(&fdbCLIEnvList, "fdbcli-env-list", "List of environment variable names (separated by comma) to retain before fork/exec'ing fdbcli")

	flag.StringVar(&mtlsFlags.ClientCertFile, "client-cert", mtlsFlags.ClientCertFile, "Path to this client's x509 cert, PEM format")
	flag.StringVar(&mtlsFlags.ClientKeyFile, "client-key", mtlsFlags.ClientKeyFile, "Path to this client's key")
	flag.StringVar(&mtlsFlags.ServerCertFile, "server-cert", mtlsFlags.ServerCertFile, "Path to an x509 server cert, PEM format")
	flag.StringVar(&mtlsFlags.ServerKeyFile, "server-key", mtlsFlags.ServerKeyFile, "Path to the server's TLS key")
	flag.StringVar(&mtlsFlags.RootCAFile, "root-ca", mtlsFlags.RootCAFile, "The root of trust for remote identities, PEM format")

	flag.StringVar(&ansible.AnsiblePlaybookBin, "ansible_playbook_bin", ansible.AnsiblePlaybookBin, "Path to ansible-playbook binary")

	flag.StringVar(&packages.YumBin, "yum-bin", packages.YumBin, "Path to yum binary")

	flag.StringVar(&process.JstackBin, "jstack-bin", process.JstackBin, "Path to the jstack binary")
	flag.StringVar(&process.JmapBin, "jmap-bin", process.JmapBin, "Path to the jmap binary")
	flag.StringVar(&process.PsBin, "ps-bin", process.PsBin, "Path to the ps binary")
	flag.StringVar(&process.PstackBin, "pstack-bin", process.PstackBin, "Path to the pstack binary")
	flag.StringVar(&process.GcoreBin, "gcore-bin", process.GcoreBin, "Path to the gcore binary")

	flag.BoolVar(&version, "version", false, "Returns the server built version from the sansshell server package")
}

func main() {
	flag.Parse()
	if err := services.SetAPIVersion(*apiVersion); err != nil {
		log.Fatalf("Unable to set API version: %v\n", err)
	}

	if version {
		fmt.Printf("Version: %s\n", ssserver.Version)
		os.Exit(0)
	}

	logOpts := log.Ldate | log.Ltime | log.Lshortfile
	logger := stdr.New(log.New(os.Stderr, "", logOpts)).WithName("sanshell-server")
	stdr.SetVerbosity(*verbosity)

	// Setup exporter using the default prometheus registry
	exporter, err := prometheus_exporter.New()
	if err != nil {
		log.Fatalf("failed to create prometheus exporter: %v\n", err)
	}
	otel.SetMeterProvider(otelmetricsdk.NewMeterProvider(
		otelmetricsdk.WithReader(exporter),
	))
	meter := otel.Meter("sansshell-server")
	recorder, err := metrics.NewOtelRecorder(meter, metrics.WithMetricNamePrefix("sansshell-server"))
	if err != nil {
		log.Fatalf("failed to create OtelRecorder: %v\n", err)
	}

	policy := util.ChoosePolicy(logger, defaultPolicy, *policyFlag, *policyFile)
	ctx := logr.NewContext(context.Background(), logger)
	ctx = metrics.NewContextWithRecorder(ctx, recorder)

	authzPolicy, err := opa.NewOpaAuthzPolicy(ctx, policy, opa.WithDenialHintsQuery("data.sansshell.authz.denial_hints"))
	if err != nil {
		log.Fatalf("Invalid policy: %v\n", err)
	}

	if *validate {
		fmt.Println("Policy passes.")
		os.Exit(0)
	}

	server.Run(ctx,
		server.WithLogger(logger),
		server.WithCredSource(*credSource),
		server.WithHostPort(*hostport),
		server.WithUnixSocket(*unixSocket),
		server.WithAuthzPolicy(authzPolicy),
		server.WithJustification(*justification),
		server.WithAuthzHook(rpcauth.PeerPrincipalFromCertHook()),
		server.WithAuthzHook(mpa.ServerMPAAuthzHook()),
		server.WithRawServerOption(func(s *grpc.Server) { reflection.Register(s) }),
		server.WithRawServerOption(func(s *grpc.Server) { channelz.RegisterChannelzServiceToServer(s) }),
		server.WithDebugPort(*debugport),
		server.WithMetricsPort(*metricsport),
		server.WithMetricsRecorder(recorder),
		server.WithRefreshCredsOnSIGHUP(),
	)
}
