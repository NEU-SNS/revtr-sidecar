// example-eventsocket-client is a minimal reference implementation of a tcpinfo
// eventsocket client.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"revtr-sidecar-mlab/log"
	revtrpb "revtr-sidecar-mlab/revtr/pb"
	"strconv"
	"time"

	"github.com/m-lab/go/flagx"
	"github.com/m-lab/go/rtx"
	"github.com/m-lab/tcp-info/eventsocket"
	"github.com/m-lab/tcp-info/inetdiag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var (
	mainCtx, mainCancel = context.WithCancel(context.Background())
	
	logger = log.GetLogger()

	revtrGRPCPort = flag.String("revtr.grpcPort", "", "The gRPC port of the revtr API")  
	// revtrAPIKey is the M-Lab API key used to call revtr API
	revtrAPIKey = flag.String("revtr.APIKey", "", "The API key used by the M-Lab nodes to call the revtr API")
	// revtrHostname is the hostname of the server hosting the Revtr API
	revtrHostname = flag.String("revtr.hostname", "", "The hostname of the revtr API server")
)

// event contains fields for an open event.
type event struct {
	timestamp time.Time
	uuid      string
	id        *inetdiag.SockID
}

// handler implements the eventsocket.Handler interface.
type handler struct {
	events chan event
}

// Open is called by tcp-info synchronously for every TCP open event.
func (h *handler) Open(ctx context.Context, timestamp time.Time, uuid string, id *inetdiag.SockID) {
	// NOTE: Until this function returns, tcp-info cannot send additional
	// events. So, immediately attempt to send the event on a channel that will
	// be read by an asynchronous processing goroutine.
	select {
	case h.events <- event{timestamp: timestamp, uuid: uuid, id: id}:
		log.Println("open ", "sent", uuid, timestamp, id)
	default:
		// If the write to the events channel would have blocked, discard this
		// event instead.
		log.Println("open ", "skipped", uuid, timestamp, id)
	}
}

// Close is called by tcp-info synchronously for every TCP close event.
func (h *handler) Close(ctx context.Context, timestamp time.Time, uuid string) {
	log.Println("close", uuid, timestamp)
}

func callRevtr(client *revtrpb.RevtrClient, revtrMeasurements []*revtrpb.RevtrMeasurement, revtrAPIKey string) {
	// Put a timeout in context
	ctx, _ := context.WithTimeout(context.Background(), time.Second*30)
	logger.Debugf("Sending %d reverse traceroutes to the revtr server...", len(revtrMeasurements))
	_, err := (*client).RunRevtr(ctx, &revtrpb.RunRevtrReq{
		Revtrs : revtrMeasurements,
		Auth: revtrAPIKey,
		CheckDB: false,
	})
	if err != nil {
		logger.Error(err)
	}

} 

// ProcessOpenEvents reads and processes events received by the open handler.
func (h *handler) ProcessOpenEvents(ctx context.Context, revtrAPIKey string, revtrHostname string, revtrGRPCPort int) {

	// Create a gRPC revtr client 
	creds := credentials.NewTLS(&tls.Config{
		InsecureSkipVerify: true,

	})
	// if err != nil {
	// 	log.Fatal(err)
	// 	return nil, err
	// }

	grpcDialOptions := []grpc.DialOption{}
	// connStr := fmt.Sprintf("%s:%d", srvs[0].Target, srvs[0].Port)
	connStr := fmt.Sprintf("%s:%d", revtrHostname, revtrGRPCPort)
	// grpcDialOptions = append(grpcDialOptions, grpc.WithInsecure())
	grpcDialOptions = append(grpcDialOptions, grpc.WithTransportCredentials(creds))
	conn, err := grpc.Dial(connStr, grpcDialOptions...)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	client := revtrpb.NewRevtrClient(conn)

	// Skeleton for a revtr measurement coming from M-Lab

	revtrMeasurement := revtrpb.RevtrMeasurement{
		Src: "", // Filled later
		Dst: "", // Filled later
		// Staleness is for the staleness of the atlas
		RrVpSelectionAlgorithm: "ingress_cover",
		UseTimestamp: false, 
		UseCache : true,
		AtlasOptions: &revtrpb.AtlasOptions{
			UseAtlas: true,
			UseRrPings: true,
			IgnoreSource: false,
			IgnoreSourceAs: false,
			StalenessBeforeRefresh: 1, // unused
			Staleness: 60 * 24, // Staleness of traceroute atlas in minutes, one day
			Platforms: []string{"mlab", "ripe"},
			
		},
		CheckDestBasedRoutingOptions: &revtrpb.CheckDestBasedRoutingOptions{
			CheckTunnel: false,
		},
		HeuristicsOptions: &revtrpb.RRHeuristicsOptions {
			UseDoubleStamp: false, 
		},
		MaxSpoofers: uint32(10),
		Label: "mlab_ndt",
		IsRunForwardTraceroute: false,
		IsRunRttPings: true,
	}


	t := time.NewTicker(30 * time.Second)

	revtrsToSend := [] * revtrpb.RevtrMeasurement{}

	for {
		select {
		case e := <-h.events:
			log.Println("processing", e)
			// Call the gRPC API of Reverse Traceroute
			
			revtrMeasurementToSend := new(revtrpb.RevtrMeasurement)
			*revtrMeasurementToSend = revtrMeasurement
			revtrMeasurementToSend.Src = e.id.SrcIP
			revtrMeasurementToSend.Dst = e.id.DstIP
			logger.Debugf("Adding reverse traceroute with source %s and destination %s to send", revtrMeasurementToSend.Src, revtrMeasurementToSend.Dst)
			revtrsToSend = append(revtrsToSend, revtrMeasurementToSend)
		
		case <- t.C:
			if len(revtrsToSend) > 0 {
				// Flush what we can flush 
				log.Infof("Sending batch of %d revtrs", len(revtrsToSend))
				revtrs := make([]*revtrpb.RevtrMeasurement, len(revtrsToSend))
				copy(revtrs, revtrsToSend)
				go callRevtr(&client, revtrsToSend, revtrAPIKey)
				revtrsToSend = nil
			}
		case <-ctx.Done():
			log.Println("shutdown")
			return
		}
	}
}

func main() {
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

	h := &handler{events: make(chan event)}

	// Process events received by the eventsocket handler. The goroutine will
	// block until an open event occurs or the context is cancelled.
	go h.ProcessOpenEvents(mainCtx, *revtrAPIKey, *revtrHostname, revtrGRPCPortInt)

	// Begin listening on the eventsocket for new events, and dispatch them to
	// the given handler.
	go eventsocket.MustRun(mainCtx, *eventsocket.Filename, h)

	<-mainCtx.Done()
}
