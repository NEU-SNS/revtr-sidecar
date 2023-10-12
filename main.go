// example-eventsocket-client is a minimal reference implementation of a tcpinfo
// eventsocket client.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"revtr-sidecar/log"
	revtrpb "revtr-sidecar/revtr/pb"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/m-lab/go/flagx"
	"github.com/m-lab/go/rtx"
	"github.com/m-lab/tcp-info/eventsocket"
	"github.com/m-lab/tcp-info/inetdiag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	// Monitoring
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	mainCtx, mainCancel = context.WithCancel(context.Background())

	logger = log.GetLogger()

	revtrGRPCPort = flag.String("revtr.grpcPort", "", "The gRPC port of the revtr API")
	// revtrAPIKey is the M-Lab API key used to call revtr API
	revtrAPIKey = flag.String("revtr.APIKey", "", "The API key used by the M-Lab nodes to call the revtr API")
	// revtrHostname is the hostname of the server hosting the Revtr API
	revtrHostname  = flag.String("revtr.hostname", "", "The hostname of the revtr API server")
	revtrSampling  = flag.Int("revtr.sampling", 0, "Only run 1 / revtr.sample revtrs to not overload the system")
	prometheusPort = flag.Int("prometheus.port", 2112, "Prometheus port to run on")

	revtrTestSrc  = "129.10.113.200"
	revtrTestSite = "fring2"
)

var (
	revtrAPICallsMetric = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "revtr_api_calls_total",
		Help: "Reverse Traceroute API calls to the Revtr system",
	},
		[]string{"status"})

	revtrSamplesMetric = promauto.NewCounter(prometheus.CounterOpts{
		Name: "revtr_samples_total",
		Help: "Reverse Traceroute measurements sent to the Revtr system",
	})

	revtrAPIRequestDurationHist = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "revtr_api_request_duration_seconds",
		Buckets: []float64{0.05, 0.1, 0.2, 0.4, 1, 3},
		Help:    "Reverse Traceroute API request duration",
	})

	revtrEventProcessingDurationHist = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "revtr_event_processing_duration_seconds",
		Buckets: []float64{2, 4, 6, 8, 10, 12, 14, 16},
		Help:    "Reverse Traceroute end-to-end event processing duration",
	})

	revtrEventsProcessedMetric = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "revtr_eventsocket_events_processed_total",
			Help: "Reverse Traceroute eventsocket events processed by event type",
		}, []string{"event"},
	)

	revtrEventsSkippedMetric = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "revtr_eventsocket_events_skipped_total",
			Help: "Reverse Traceroute eventsocket events skipped by event type",
		}, []string{"event"},
	)
)

// event contains fields for an open event.
type event struct {
	timestamp time.Time
	uuid      string
	id        *inetdiag.SockID
	*revtrpb.RevtrMeasurement
}

// handler implements the eventsocket.Handler interface.
type handler struct {
	events           chan event
	mlabIPtoSite     map[string]string
	mlabIPToSiteLock sync.RWMutex
}

// Open is called by tcp-info synchronously for every TCP open event.
func (h *handler) Open(ctx context.Context, timestamp time.Time, uuid string, id *inetdiag.SockID) {
	// NOTE: Until this function returns, tcp-info cannot send additional
	// events. So, immediately attempt to send the event on a channel that will
	// be read by an asynchronous processing goroutine.
	select {
	case h.events <- event{timestamp: timestamp, uuid: uuid, id: id}:
		revtrEventsProcessedMetric.WithLabelValues("open").Inc()
		log.Println("open ", "sent", uuid, timestamp, id)
	default:
		// If the write to the events channel would have blocked, discard this
		// event instead.
		revtrEventsSkippedMetric.WithLabelValues("open").Inc()
		log.Println("open ", "skipped", uuid, timestamp, id)
	}
}

// Close is called by tcp-info synchronously for every TCP close event.
func (h *handler) Close(ctx context.Context, timestamp time.Time, uuid string) {
	log.Println("close", uuid, timestamp)
}

func callRevtr(client *revtrpb.RevtrClient, events []event, revtrAPIKey string, revtrSampling int) {
	// Extract revtr measurements from events and record e2e processing time.
	revtrMeasurements := make([]*revtrpb.RevtrMeasurement, len(events))
	for i, event := range events {
		revtrMeasurements[i] = event.RevtrMeasurement
		revtrEventProcessingDurationHist.Observe(time.Since(event.timestamp).Seconds())
	}

	// Put a timeout in context
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	start := time.Now()
	_, err := (*client).RunRevtr(ctx, &revtrpb.RunRevtrReq{
		Revtrs:  revtrMeasurements,
		Auth:    revtrAPIKey,
		CheckDB: false,
	})
	elapsed := time.Since(start)
	log.Debugf("revtr API request took %d ms", elapsed.Milliseconds())
	revtrAPIRequestDurationHist.Observe(elapsed.Seconds())
	if err != nil {
		revtrAPICallsMetric.WithLabelValues("error").Inc()
		logger.Error(err)
	}
	revtrAPICallsMetric.WithLabelValues("success").Inc()
}

type MLabNode struct {
	Hostname string `json:"hostname,omitempty"`
	IPv4     string `json:"ipv4,omitempty"`
}

func getMLabNodes(mlabNodesURL string) (map[string]string, error) {
	url := mlabNodesURL

	resp, err := http.Get(url)
	if err != nil {
		fmt.Println("Error:", err)
		return nil, err
	}
	defer resp.Body.Close()

	var results []MLabNode
	err = json.NewDecoder(resp.Body).Decode(&results)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	// Now parse the map to have IPtoSite for MLab and SiteToIPs for Revtr
	mlabIPtoSite := map[string]string{}

	for _, mlabNode := range results {
		// Parse the value to get the site
		if strings.HasPrefix(mlabNode.Hostname, "ndt") ||
			strings.HasPrefix(mlabNode.Hostname, "wehe") {
			mlabSiteType := strings.Split(mlabNode.Hostname, ".")[0]
			mlabSiteTypeSplit := strings.Split(mlabSiteType, "-")
			// There are two types of NDT nodes, o ne with a iupui string?
			var site string
			if strings.Contains(mlabSiteType, "iupui") {
				site = mlabSiteTypeSplit[3]
			} else {
				site = mlabSiteTypeSplit[2]
			}
			mlabIPtoSite[mlabNode.IPv4] = site
		}
		// else if strings.HasPrefix(mlabNode.Hostname, "mlab") {
		// 	mlabSiteType := strings.Split(mlabNode.Hostname, ".")[0]
		// 	mlabSiteTypeSplit := strings.Split(mlabSiteType, "-")
		// 	site := mlabSiteTypeSplit[1]
		// 	mlabIPtoSite[mlabNode.IPv4] = site
		// }
	}

	// Add the test site for testing
	mlabIPtoSite[revtrTestSrc] = revtrTestSite

	return mlabIPtoSite, nil

}

// ProcessOpenEvents reads and processes events received by the open handler.
func (h *handler) ProcessOpenEvents(ctx context.Context, revtrAPIKey string, revtrHostname string,
	revtrGRPCPort int, revtrSampling int) {

	grpcDialOptions := []grpc.DialOption{}
	connStr := fmt.Sprintf("%s:%d", revtrHostname, revtrGRPCPort)
	if revtrGRPCPort == 9998 {
		// This is debug port, so no tls connection
		grpcDialOptions = append(grpcDialOptions, grpc.WithInsecure())
	} else {
		creds := credentials.NewTLS(&tls.Config{
			InsecureSkipVerify: true,
		})
		grpcDialOptions = append(grpcDialOptions, grpc.WithTransportCredentials(creds))
	}

	conn, err := grpc.Dial(connStr, grpcDialOptions...)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	client := revtrpb.NewRevtrClient(conn)

	sourcesWithAtlas, err := client.GetSources(context.Background(), &revtrpb.GetSourcesReq{
		Auth:          revtrAPIKey,
		OnlyWithAtlas: true})
	if err != nil {
		log.Error(err)
		return
	}
	revtrSiteToIP := map[string]string{}

	for _, sourceWithAtlas := range sourcesWithAtlas.Srcs {
		// Transform site from M-Lab to site
		site := strings.Split(sourceWithAtlas.Site, "MLab - ")[1]
		revtrSiteToIP[site] = sourceWithAtlas.Ip
	}
	revtrSiteToIP[revtrTestSite] = revtrTestSrc
	// Skeleton for a revtr measurement coming from M-Lab

	t := time.NewTicker(15 * time.Second)

	eventsToSend := []event{}
	i := 0

	for {
		select {
		case e := <-h.events:
			log.Println("processing", e.uuid)
			// Call the gRPC API of Reverse Traceroute
			// Match the sources with the mapping of the revtr sites / IP addresses
			// Check if we have a source in the same site as the NDT
			if site, ok := h.mlabIPtoSite[e.id.SrcIP]; ok {
				if revtrSrc, ok := revtrSiteToIP[site]; ok {
					if i%revtrSampling == 0 {
						// Sample the event.
						e.RevtrMeasurement = newRevtrMeasurement(revtrSrc, e.id.DstIP, e.uuid)
						eventsToSend = append(eventsToSend, e)
						revtrSamplesMetric.Inc()
						logger.Debugf("Adding reverse traceroute with source %s and destination %s to send",
							revtrSrc, e.id.DstIP)
					}
					i++
				} else {
					log.Infof("Site %s IP %s is not a revtr site", site, e.id.SrcIP)
				}
			} else {
				log.Infof("No NDT site matching for IP %s", e.id.SrcIP)
			}
		case <-t.C:
			if len(eventsToSend) > 0 {
				// Flush what we can flush
				log.Infof("Collected batch of %d revtrs to send (sampling 1/%d)", len(eventsToSend), revtrSampling)
				events := make([]event, len(eventsToSend))
				copy(events, eventsToSend)
				go callRevtr(&client, events, revtrAPIKey, revtrSampling)
				eventsToSend = nil
			}
		case <-ctx.Done():
			log.Println("shutdown")
			return
		}
	}
}

// newRevtrMeasurement creates a new revtr measurement with the given src, dst and uuid.
func newRevtrMeasurement(src, dst, uuid string) *revtrpb.RevtrMeasurement {
	return &revtrpb.RevtrMeasurement{
		Src:  src,
		Dst:  dst,
		Uuid: uuid,

		// Staleness is for the staleness of the atlas
		RrVpSelectionAlgorithm: "ingress_cover",
		UseTimestamp:           false,
		UseCache:               true,
		AtlasOptions: &revtrpb.AtlasOptions{
			UseAtlas:               true,
			UseRrPings:             true,
			IgnoreSource:           false,
			IgnoreSourceAs:         false,
			StalenessBeforeRefresh: 1,       // unused
			Staleness:              60 * 24, // Staleness of traceroute atlas in minutes, one day
			Platforms:              []string{"mlab", "ripe"},
		},
		CheckDestBasedRoutingOptions: &revtrpb.CheckDestBasedRoutingOptions{
			CheckTunnel: false,
		},
		HeuristicsOptions: &revtrpb.RRHeuristicsOptions{
			UseDoubleStamp: false,
		},
		MaxSpoofers:            uint32(10),
		Label:                  "ndt_revtr_sidecar",
		IsRunForwardTraceroute: false,
		IsRunRttPings:          true,
	}
}

func refreshMLabNodes(h *handler) {

	t := time.NewTicker(time.Hour * 6)

	url := "https://siteinfo.mlab-oti.measurementlab.net/v2/sites/hostnames.json"
	h.mlabIPToSiteLock.Lock()
	mlabIPtoSite, err := getMLabNodes(url)
	if err != nil {
		log.Error(err)
	} else {
		h.mlabIPtoSite = mlabIPtoSite
	}
	h.mlabIPToSiteLock.Unlock()

	for range t.C {
		log.Infof("Refreshing MLab nodes")
		h.mlabIPToSiteLock.Lock()
		mlabIPtoSite, err := getMLabNodes(url)
		if err != nil {
			log.Error(err)
		} else {
			h.mlabIPtoSite = mlabIPtoSite
		}
		h.mlabIPToSiteLock.Unlock()
	}

}

func main() {

	h := &handler{events: make(chan event)}
	// getMLabNodes("https://siteinfo.mlab-oti.measurementlab.net/v2/sites/hostnames.json")
	go refreshMLabNodes(h)

	defer mainCancel()

	flag.Parse()
	rtx.Must(flagx.ArgsFromEnv(flag.CommandLine), "could not get args from environment variables")

	if *eventsocket.Filename == "" {
		log.Fatal("-tcpinfo.eventsocket path is required")
	}

	if *revtrAPIKey == "" {
		log.Fatal("-revtr.APIKey is required")
	}

	if *revtrHostname == "" {
		log.Fatal("-revtr.hostname is required")
	}

	if *revtrSampling == 0 {
		log.Fatal("-revtr.sampling is required and must be > 0")
	}

	var revtrGRPCPortInt int
	var err error
	if *revtrGRPCPort == "" {
		log.Fatal("-revtr.grpcPort is required")
	} else {
		revtrGRPCPortInt, err = strconv.Atoi(*revtrGRPCPort)
		if err != nil {
			log.Fatal("Bad argument revtr.grpcPort")
		}
	}

	// Process events received by the eventsocket handler. The goroutine will
	// block until an open event occurs or the context is cancelled.
	go h.ProcessOpenEvents(mainCtx, *revtrAPIKey, *revtrHostname, revtrGRPCPortInt, *revtrSampling)

	// Begin listening on the eventsocket for new events, and dispatch them to
	// the given handler.
	go eventsocket.MustRun(mainCtx, *eventsocket.Filename, h)

	http.Handle("/metrics", promhttp.Handler())
	go func() {
		http.ListenAndServe(":"+strconv.FormatInt(int64(*prometheusPort), 10), nil)
	}()

	<-mainCtx.Done()
}
