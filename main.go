package main

import (
	"context"
	"crypto/tls"
	"fmt"
	stdlog "log"
	"net/http/httptrace"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/grpclog"

	tlshelpers "github.com/cloudflare/cfssl/helpers"
	"github.com/davecgh/go-spew/spew"

	"github.com/alecthomas/kingpin"
	"github.com/coreos/etcd/clientv3"
	kitlog "github.com/go-kit/kit/log"
	"github.com/montanaflynn/stats"
)

var logger kitlog.Logger

var (
	app                = kingpin.New("etcd-latency", "").Version("0.0.1")
	grpcDebugLogs      = app.Flag("grpc-debug-logs", "enables grpc debug logging").Bool()
	syncEndpoints      = app.Flag("sync-endpoints", "attempts to sync endpoints with etcd cluster").Bool()
	sleep              = app.Flag("sleep", "sleep for this long after opening connection").Default("0s").Duration()
	count              = app.Flag("count", "number of benchmark runs").Default("100").Int()
	endpoints          = app.Flag("endpoints", "comma separated etcd endpoint list").Default("127.0.0.1:2379").Envar(`ETCDCTL_ENDPOINTS`).String()
	dialTimeout        = app.Flag("dial-timeout", "dial timeout for client connections").Default("5s").Envar(`ETCDCTL_DIAL_TIMEOUT`).Duration()
	keepaliveTime      = app.Flag("keepalive-time", "keepalive time for client connections").Default("2s").Envar(`ETCDCTL_KEEPALIVE_TIME`).Duration()
	keepaliveTimeout   = app.Flag("keepalive-timeout", "keepalive timeout for client connections").Envar(`ETCDCTL_KEEPALIVE_TIMEOUT`).Default("6s").Duration()
	insecure           = app.Flag("insecure", "disable transport security for client connections").Envar(`ETCDCTL_INSECURE`).Bool()
	insecureSkipVerify = app.Flag("insecure-skip-verify", "accept insecure SRV records describing cluster endpoints").Envar(`ETCDCTL_INSECURE_SKIP_TLS_VERIFY`).Bool()
	certFile           = app.Flag("cert", "identify secure client using this TLS certificate file").Envar(`ETCDCTL_CERT`).String()
	keyFile            = app.Flag("key", "identify secure client using this TLS key file").Envar(`ETCDCTL_KEY`).String()
	cacertFile         = app.Flag("cacert", "verify certificates of TLS-enabled secure servers using this CA bundle").Envar(`ETCDCTL_CACERT`).String()
)

type kitlogWriter struct{ kitlog.Logger }

func (l kitlogWriter) Write(p []byte) (int, error) {
	return 0, l.Log("msg", string(p))
}

func main() {
	kingpin.MustParse(app.Parse(os.Args[1:]))

	logger = kitlog.NewLogfmtLogger(kitlog.NewSyncWriter(os.Stderr))
	logger = kitlog.With(logger, "ts", kitlog.DefaultTimestampUTC)
	stdlog.SetOutput(kitlog.NewStdlibAdapter(logger))

	if *grpcDebugLogs {
		var out = kitlogWriter{logger}
		clientv3.SetLogger(
			grpclog.NewLoggerV2WithVerbosity(out, out, out, 0),
		)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal)
	signal.Notify(sigs, syscall.SIGTERM)
	go func() {
		<-sigs
		logger.Log("msg", "received signal, shutting down")
		cancel()
	}()

	tlsCfg, err := newTLS(*certFile, *keyFile, *cacertFile, *insecureSkipVerify)
	if err != nil {
		kingpin.Fatalf("invalid tls: %v", err)
	}

	var start time.Time
	var trace = httptrace.ClientTrace{
		DNSStart: func(_ httptrace.DNSStartInfo) {
			logger.Log("trace", "dns_start", "duration", time.Since(start))
		},
		DNSDone: func(_ httptrace.DNSDoneInfo) {
			logger.Log("trace", "dns_done", "duration", time.Since(start))
		},
		ConnectDone: func(network, addr string, err error) {
			logger.Log("trace", "connect_done", "addr", addr, "error", err, "duration", time.Since(start))
		},
		TLSHandshakeStart: func() {
			logger.Log("trace", "tls_handshake_start", "duration", time.Since(start))
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			logger.Log("trace", "tls_handshake_done", "state", state, "duration", time.Since(start))
		},
		GotFirstResponseByte: func() {
			logger.Log("trace", "got_first_byte", "duration", time.Since(start))
		},
	}

	ctx = httptrace.WithClientTrace(ctx, &trace)
	start = time.Now()

	client, err := clientv3.New(
		clientv3.Config{
			Context:              ctx,
			TLS:                  tlsCfg,
			Endpoints:            strings.Split(*endpoints, ","),
			DialTimeout:          *dialTimeout,
			DialKeepAliveTime:    *keepaliveTime,
			DialKeepAliveTimeout: *keepaliveTimeout,
		},
	)

	logger.Log("trace", "connection_established", "duration", time.Since(start))

	if err != nil {
		kingpin.Fatalf("failed to connect to etcd: %s", err)
	}

	if *syncEndpoints {
		if err := client.Sync(ctx); err != nil {
			kingpin.Fatalf("failed to sync endpoints: %s", err)
		}
	}

	logger.Log("endpoints", strings.Join(client.Endpoints(), ","))

	if *sleep > 0 {
		ctx, cancel := context.WithCancel(ctx)
		go func(ctx context.Context) {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(100 * time.Millisecond):
					if _, err := client.Get(ctx, "nothing"); err != nil {
						logger.Log("error", err, "msg", "sleeping health check failed")
					}
				}
			}
		}(ctx)

		logger.Log("event", "sleeping", "duration", sleep.Seconds())
		time.Sleep(*sleep)

		cancel()
	}

	spew.Dump(
		runBenchmarks(ctx, *count, map[string]func(context.Context) error{
			"linearisable get for missing key": func(ctx context.Context) error {
				_, err := client.Get(ctx, "nothing")
				return err
			},
			"serializable get for missing key": func(ctx context.Context) error {
				_, err := client.Get(ctx, "nothing", clientv3.WithSerializable())
				return err
			},
		}),
	)
}

func runBenchmarks(ctx context.Context, count int, ops map[string]func(context.Context) error) map[string]interface{} {
	var results = map[string]interface{}{}
	for name, op := range ops {
		results[name] = runBenchmark(ctx, count, op)
	}

	return results
}

func runBenchmark(ctx context.Context, count int, op func(context.Context) error) interface{} {
	var errors float64
	var timings = []float64{}

	for i := 0; i < count; i++ {
		start := time.Now()
		if err := op(ctx); err != nil {
			errors++
			logger.Log("error", err)
		}

		timings = append(timings, float64(time.Since(start)))
	}

	must := func(res float64, _ error) time.Duration { return time.Duration(res) }

	return struct {
		mean, p50, p75, p90, p95 time.Duration
		errorRate                float64
	}{
		mean:      must(stats.Mean(timings)),
		p50:       must(stats.Percentile(timings, 50)),
		p75:       must(stats.Percentile(timings, 75)),
		p90:       must(stats.Percentile(timings, 90)),
		p95:       must(stats.Percentile(timings, 95)),
		errorRate: errors / float64(count),
	}
}

func newTLS(certFile, keyFile, cacertFile string, skipTLSVerify bool) (*tls.Config, error) {
	if certFile == "" && keyFile == "" && cacertFile == "" {
		return nil, nil // no TLS required
	}

	var cfg = &tls.Config{}

	if certFile != "" && keyFile != "" {
		cert, err := tlshelpers.LoadClientCertificate(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certs: %s", err)
		}

		cfg.Certificates = []tls.Certificate{*cert}
	}

	if cacertFile != "" {
		roots, err := tlshelpers.LoadPEMCertPool(cacertFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load CA file: %s", err)
		}

		cfg.RootCAs = roots
	}

	cfg.InsecureSkipVerify = skipTLSVerify

	return cfg, nil
}
