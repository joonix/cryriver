// Package cryriver is used for indexing mongodb objects into elasticsearch in real time.
package main

import (
	"flag"
	"fmt"
	"github.com/duego/cryriver/elasticsearch"
	"github.com/duego/cryriver/mongodb"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
)

var (
	mongoServer  = flag.String("mongo", "localhost", "Specific server to tail")
	mongoInitial = flag.Bool(
		"initial",
		false,
		"True if we want to do initial sync from the full collection, otherwise resume reading oplog")
	esServer      = flag.String("es", "http://localhost:9200", "Elasticsearch server to index to")
	esConcurrency = flag.Int("concurrency", 1, "Maximum number of simultaneous ES connections")
	esIndex       = flag.String("index", "testing", "Elasticsearch index to use")
	optimeStore   = flag.String(
		"db", "/tmp/cryriver.db", "What file to save progress on for oplog resumes")
	ns        = flag.String("ns", "api.users", "The namespace to tail on")
	debugAddr = flag.String(
		"debug", "127.0.0.1:5000", "Which address to listen on for debug, empty for no debug")
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	flag.Parse()
	log.SetFlags(log.Lshortfile | log.LstdFlags)

	// Enable http server for debug endpoint
	go func() {
		if *debugAddr != "" {
			log.Println(http.ListenAndServe(*debugAddr, nil))
		}
	}()

	interrupt := make(chan os.Signal)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	mongoc := make(chan *mongodb.Operation)
	closingMongo := make(chan chan error)
	go mongodb.Tail(*mongoServer, *ns, *mongoInitial, lastEsSeen, mongoc, closingMongo)

	esc := make(chan elasticsearch.Transaction)
	esDone := make(chan bool)
	go func() {
		// Boot up our slurpers.
		// The client will have the transport configured to allow the same amount of connections
		// as go routines towards ES, each connection may be re-used between slurpers.
		client := elasticsearch.NewClient(fmt.Sprintf("%s/_bulk", *esServer), *esConcurrency)
		var slurpers sync.WaitGroup
		for n := 0; n < *esConcurrency; n++ {
			slurpers.Add(1)
			go func() {
				elasticsearch.Slurp(client, esc)
				slurpers.Done()
			}()
		}
		slurpers.Wait()
		close(esDone)
	}()

	mongoDone := make(chan bool)
	go func() {
		// Map mongo collections to es index
		indexes := map[string]string{
			strings.Split(*ns, ".")[0]: *esIndex,
		}
		// Wrap all mongo operations to comply with ES interface, then send them off to the slurper.
		for op := range mongoc {
			esc <- &mongodb.EsOperation{
				Operation:    op,
				Manipulators: mongodb.DefaultManipulators,
				IndexMap:     indexes,
			}
			lastEsSeenC <- &op.Timestamp
		}
		close(mongoDone)
	}()

	select {
	//  Get more operations from mongo tail
	case <-mongoDone:
		log.Println("MongoDB tailer returned")
	// ES client closed
	case <-esDone:
		log.Println("ES slurper returned")
	// An interrupt signal was catched
	case <-interrupt:
		log.Println("Closing down...")
	}

	// MongoDB tailer shutdown
	errc := make(chan error)
	closingMongo <- errc
	if err := <-errc; err != nil {
		log.Println(err)
	} else {
		log.Println("No errors occured in mongo tail")
	}

	// Elasticsearch indexer shutdown
	close(esc)
	log.Println("Waiting for ES to return")
	<-esDone
	log.Println("Bye!")
}
