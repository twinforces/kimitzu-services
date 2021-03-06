package p2p

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/pprof"
	_ "net/http/pprof"
	"time"

	"github.com/gorilla/mux"
    "github.com/gorilla/websocket"
	"github.com/perlin-network/noise/skademlia"

	jsoniter "github.com/json-iterator/go"

	"github.com/nokusukun/particles/satellite"

	"github.com/kimitzu/kimitzu-services/models"
)

var json = jsoniter.ConfigCompatibleWithStandardLibrary
var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool {
        return true
    },
}

type WriteRequest struct {
	PacketType  string      `json:"type"`
	Destination string      `json:"destination"`
	Namespace   string      `json:"namespace"`
	Content     interface{} `json:"content"`
}

func setupResponse(w *http.ResponseWriter, req *http.Request) bool {
	(*w).Header().Set("Access-Control-Allow-Origin", req.Header.Get("origin"))
	(*w).Header().Set("Access-Control-Allow-Methods", "POST, GET, PATCH, PUT, DELETE, OPTIONS")
	(*w).Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, Origin, X-Requested-With")
	(*w).Header().Set("Access-Control-Allow-Credentials", "true")
	(*w).Header().Set("Content-Type", "application/json")
	if req.Method == "OPTIONS" {
		(*w).WriteHeader(http.StatusOK)
		return true
	}
	return false
}

func AttachAPI(sat *satellite.Satellite, router *mux.Router, manager *RatingManager) *mux.Router {
	// router := mux.NewRouter()

	router.HandleFunc("/debug/pprof/", pprof.Index)
	router.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	router.HandleFunc("/debug/pprof/profile", pprof.Profile)
	router.HandleFunc("/debug/pprof/symbol", pprof.Symbol)

	// Manually add support for paths linked to by index page at /debug/pprof/
	router.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	router.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	router.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	router.Handle("/debug/pprof/block", pprof.Handler("block"))

	router.HandleFunc("/p2p/peers", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("Retieving Peers")
		var ids []string

		for id, _ := range sat.Peers {
			ids = append(ids, id)
		}

		_ = json.NewEncoder(w).Encode(ids)
	}).Methods("GET")

    router.HandleFunc("/p2p/ratings/seek/{ids}", func(w http.ResponseWriter, r *http.Request) {
        vars := mux.Vars(r)
        ws, err := upgrader.Upgrade(w, r, nil)
        if err != nil {
            fmt.Println("Failed to upgrade to websocket connection:", r.RemoteAddr)
            _ = json.NewEncoder(w).Encode(map[string]interface{}{
                "error": err,
            })
            return
        }

        go func() {
            rs, err := sat.Seek("get_rating", RatingRequest{vars["ids"]})
            if err != nil {
                log.Errorf("failed to broadcast: %v", err)
            } else {
                log.Debug("Waiting for streams")
                for inbound := range rs.Stream {
                    //ratings = append(ratings, inbound.Payload)
                    _ = ws.WriteJSON(inbound.Payload)
                }
                ws.Close()
            }
        }()
    })

	router.HandleFunc("/p2p/ratings/publish/{type}", func(w http.ResponseWriter, r *http.Request) {
		if retOK := setupResponse(&w, r); retOK {
			return
		}

		contract := new(models.Contract)
		publishType := mux.Vars(r)["type"]

		var err error
		b, err := ioutil.ReadAll(r.Body)
		if err != nil {
			log.Debugf("failed to read body: %v", err)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": err,
			})
			return
		}
		_ = json.Unmarshal(b, &contract)

		// Ingest Rating to internal database
		var rating *Rating
		if publishType == "fulfill" {
			rating, err = manager.IngestFulfillmentRating(contract)
		} else if publishType == "complete" {
			rating, err = manager.IngestCompletionRating(contract)
		} else {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": "endpoint only accepts either 'vendor' or 'buyer'",
			})
			return
		}

		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": fmt.Sprint(err),
			})
			return
		}

		// Broadcast to the network
		var errCode string
		errs := skademlia.Broadcast(sat.Node, satellite.Packet{
			PacketType: satellite.PType_Broadcast,
			Namespace:  "new_rating",
			Payload:    rating,
		})

		if errs != nil {
			log.Debugf("failed to broadcast: %v", err)
			errCode = fmt.Sprintf("failed to broadcast: %v", err)
		}

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": errCode,
		})
	})

	router.HandleFunc("/p2p/ratings/get/{peer}/{ids}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		var errCode string
		var ratings []interface{}

		p, exists := sat.Peers[vars["peer"]]
		if exists {
			start := time.Now()
			rs, err := sat.Request(p, "get_rating", RatingRequest{vars["ids"]})
			if err != nil {
				log.Errorf("failed to write: %v", err)
				errCode = fmt.Sprintf("failed to write: %v", err)
			} else {
				log.Debug("Waiting for streams")
				for inbound := range rs.Stream {
					ratings = append(ratings, inbound.Payload)
				}
			}
			log.Debug("Waiting for streams is complete: ", time.Now().Sub(start))
		} else {
			errCode = fmt.Sprintf("peer does not exist: %v", vars["peer"])
		}

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ratings": ratings,
			"error":   errCode,
		})
	})

	router.HandleFunc("/p2p/ratings/seek-sync/{ids}", func(w http.ResponseWriter, r *http.Request) {
		if retOK := setupResponse(&w, r); retOK {
			return
		}

		vars := mux.Vars(r)
		var errCode string
		var ratings []interface{}

		rs, err := sat.Seek("get_rating", RatingRequest{vars["ids"]})
		if err != nil {
			log.Errorf("failed to broadcast: %v", err)
			errCode = fmt.Sprintf("failed to write: %v", err)
		} else {
			log.Debug("Waiting for streams")
			for inbound := range rs.Stream {
				ratings = append(ratings, inbound.Payload)
			}
		}

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ratings": ratings,
			"error":   errCode,
		})
	})

	return router
}
