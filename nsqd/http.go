package nsqd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/nsqio/nsq/internal/http_api"
	"github.com/nsqio/nsq/internal/protocol"
	"github.com/nsqio/nsq/internal/version"
)

type httpServer struct {
	ctx         *context
	tlsEnabled  bool
	tlsRequired bool
	router      http.Handler
}

func newHTTPServer(ctx *context, tlsEnabled bool, tlsRequired bool) *httpServer {
	log := http_api.Log(ctx.nsqd.getOpts().Logger)

	router := httprouter.New()
	router.HandleMethodNotAllowed = true
	router.PanicHandler = http_api.LogPanicHandler(ctx.nsqd.getOpts().Logger)
	router.NotFound = http_api.LogNotFoundHandler(ctx.nsqd.getOpts().Logger)
	router.MethodNotAllowed = http_api.LogMethodNotAllowedHandler(ctx.nsqd.getOpts().Logger)
	s := &httpServer{
		ctx:         ctx,
		tlsEnabled:  tlsEnabled,
		tlsRequired: tlsRequired,
		router:      router,
	}

	router.Handle("GET", "/ping", http_api.Decorate(s.pingHandler, log, http_api.PlainText))

	// v1 negotiate
	router.Handle("POST", "/pub", http_api.Decorate(s.doPUB, http_api.NegotiateVersion))
	router.Handle("POST", "/mpub", http_api.Decorate(s.doMPUB, http_api.NegotiateVersion))
	router.Handle("GET", "/stats", http_api.Decorate(s.doStats, log, http_api.NegotiateVersion))
	router.Handle("GET", "/message/stats", http_api.Decorate(s.doMessageStats, log, http_api.NegotiateVersion))
	//router.Handle("POST", "/topic/pause", http_api.Decorate(s.doPauseTopic, log, http_api.V1))
	//router.Handle("POST", "/topic/unpause", http_api.Decorate(s.doPauseTopic, log, http_api.V1))
	router.Handle("POST", "/channel/pause", http_api.Decorate(s.doPauseChannel, log, http_api.V1))
	router.Handle("POST", "/channel/unpause", http_api.Decorate(s.doPauseChannel, log, http_api.V1))
	router.Handle("GET", "/config/:opt", http_api.Decorate(s.doConfig, log, http_api.V1))
	router.Handle("PUT", "/config/:opt", http_api.Decorate(s.doConfig, log, http_api.V1))

	// only v1, deprecated
	router.Handle("POST", "/topic/create", http_api.Decorate(s.doCreateTopic, http_api.DeprecatedAPI, log, http_api.V1))
	router.Handle("POST", "/topic/delete", http_api.Decorate(s.doDeleteTopic, http_api.DeprecatedAPI, log, http_api.V1))
	router.Handle("POST", "/topic/empty", http_api.Decorate(s.doEmptyTopic, http_api.DeprecatedAPI, log, http_api.V1))
	router.Handle("POST", "/channel/create", http_api.Decorate(s.doCreateChannel, http_api.DeprecatedAPI, log, http_api.V1))
	router.Handle("POST", "/channel/delete", http_api.Decorate(s.doDeleteChannel, http_api.DeprecatedAPI, log, http_api.V1))
	//router.Handle("POST", "/channel/empty", http_api.Decorate(s.doEmptyChannel, http_api.DeprecatedAPI, log, http_api.V1))

	// debug
	router.HandlerFunc("GET", "/debug/pprof/", pprof.Index)
	router.HandlerFunc("GET", "/debug/pprof/cmdline", pprof.Cmdline)
	router.HandlerFunc("GET", "/debug/pprof/symbol", pprof.Symbol)
	router.HandlerFunc("GET", "/debug/pprof/profile", pprof.Profile)
	router.Handler("GET", "/debug/pprof/heap", pprof.Handler("heap"))
	router.Handler("GET", "/debug/pprof/goroutine", pprof.Handler("goroutine"))
	router.Handler("GET", "/debug/pprof/block", pprof.Handler("block"))
	router.Handler("GET", "/debug/pprof/threadcreate", pprof.Handler("threadcreate"))

	return s
}

func (s *httpServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !s.tlsEnabled && s.tlsRequired {
		resp := fmt.Sprintf(`{"message": "TLS_REQUIRED", "https_port": %d}`,
			s.ctx.nsqd.RealHTTPSAddr().Port)
		http_api.Respond(w, 403, "", resp)
		return
	}
	s.router.ServeHTTP(w, req)
}

func (s *httpServer) pingHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	health := s.ctx.nsqd.GetHealth()
	if !s.ctx.nsqd.IsHealthy() {
		return nil, http_api.Err{500, health}
	}
	return health, nil
}

func (s *httpServer) doInfo(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, http_api.Err{500, err.Error()}
	}
	return struct {
		Version          string `json:"version"`
		BroadcastAddress string `json:"broadcast_address"`
		Hostname         string `json:"hostname"`
		HTTPPort         int    `json:"http_port"`
		TCPPort          int    `json:"tcp_port"`
		StartTime        int64  `json:"start_time"`
	}{
		Version:          version.Binary,
		BroadcastAddress: s.ctx.nsqd.getOpts().BroadcastAddress,
		Hostname:         hostname,
		TCPPort:          s.ctx.nsqd.RealTCPAddr().Port,
		HTTPPort:         s.ctx.nsqd.RealHTTPAddr().Port,
		StartTime:        s.ctx.nsqd.GetStartTime().Unix(),
	}, nil
}

func (s *httpServer) getExistingTopicFromQuery(req *http.Request) (*http_api.ReqParams, *Topic, string, error) {
	reqParams, err := http_api.NewReqParams(req)
	if err != nil {
		nsqLog.LogErrorf("failed to parse request params - %s", err)
		return nil, nil, "", http_api.Err{400, "INVALID_REQUEST"}
	}

	topicName, channelName, err := http_api.GetTopicChannelArgs(reqParams)
	if err != nil {
		return nil, nil, "", http_api.Err{400, err.Error()}
	}

	topic, err := s.ctx.nsqd.GetExistingTopic(topicName)
	if err != nil {
		nsqLog.Logf("topic not found - %s", topicName)
		return nil, nil, "", http_api.Err{404, "TOPIC_NOT_FOUND"}
	}

	return reqParams, topic, channelName, err
}

func (s *httpServer) getTopicFromQuery(req *http.Request) (url.Values, *Topic, error) {
	reqParams, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		nsqLog.LogErrorf("failed to parse request params - %s", err)
		return nil, nil, http_api.Err{400, "INVALID_REQUEST"}
	}

	topicNames, ok := reqParams["topic"]
	if !ok {
		return nil, nil, http_api.Err{400, "MISSING_ARG_TOPIC"}
	}
	topicName := topicNames[0]

	if !protocol.IsValidTopicName(topicName) {
		return nil, nil, http_api.Err{400, "INVALID_TOPIC"}
	}
	topicParts, ok := reqParams["partition"]
	topicPart := 0
	if !ok {
		return reqParams, s.ctx.nsqd.GetTopicIgnPart(topicName), nil
	} else {
		topicPart, err = strconv.Atoi(topicParts[0])
		if err != nil {
			return nil, nil, http_api.Err{400, "INVALID_ARG_TOPIC_PARTITION"}
		}
	}

	return reqParams, s.ctx.nsqd.GetTopic(topicName, topicPart), nil
}

func (s *httpServer) doPUB(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	// TODO: one day I'd really like to just error on chunked requests
	// to be able to fail "too big" requests before we even read

	if req.ContentLength > s.ctx.nsqd.getOpts().MaxMsgSize {
		return nil, http_api.Err{413, "MSG_TOO_BIG"}
	}

	// add 1 so that it's greater than our max when we test for it
	// (LimitReader returns a "fake" EOF)
	readMax := s.ctx.nsqd.getOpts().MaxMsgSize + 1
	body, err := ioutil.ReadAll(io.LimitReader(req.Body, readMax))
	if err != nil {
		nsqLog.Logf("read request body error: %v", err)
		return nil, http_api.Err{500, "INTERNAL_ERROR"}
	}
	if int64(len(body)) == readMax {
		return nil, http_api.Err{413, "MSG_TOO_BIG"}
	}
	if len(body) == 0 {
		return nil, http_api.Err{400, "MSG_EMPTY"}
	}

	_, topic, err := s.getTopicFromQuery(req)
	if err != nil {
		nsqLog.Logf("get topic err: %v", err)
		return nil, err
	}

	msg := NewMessage(0, body)
	err = topic.PutMessage(msg)
	if err != nil {
		return nil, http_api.Err{503, "EXITING"}
	}

	return "OK", nil
}

func (s *httpServer) doMPUB(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	var msgs []*Message
	var exit bool

	// TODO: one day I'd really like to just error on chunked requests
	// to be able to fail "too big" requests before we even read

	if req.ContentLength > s.ctx.nsqd.getOpts().MaxBodySize {
		return nil, http_api.Err{413, "BODY_TOO_BIG"}
	}

	reqParams, topic, err := s.getTopicFromQuery(req)
	if err != nil {
		return nil, err
	}

	_, ok := reqParams["binary"]
	if ok {
		tmp := make([]byte, 4)
		msgs, err = readMPUB(req.Body, tmp, topic,
			s.ctx.nsqd.getOpts().MaxMsgSize)
		if err != nil {
			return nil, http_api.Err{413, err.(*protocol.FatalClientErr).Code[2:]}
		}
	} else {
		// add 1 so that it's greater than our max when we test for it
		// (LimitReader returns a "fake" EOF)
		readMax := s.ctx.nsqd.getOpts().MaxBodySize + 1
		rdr := bufio.NewReader(io.LimitReader(req.Body, readMax))
		total := 0
		for !exit {
			var block []byte
			block, err = rdr.ReadBytes('\n')
			if err != nil {
				if err != io.EOF {
					return nil, http_api.Err{500, "INTERNAL_ERROR"}
				}
				exit = true
			}
			total += len(block)
			if int64(total) == readMax {
				return nil, http_api.Err{413, "BODY_TOO_BIG"}
			}

			if len(block) > 0 && block[len(block)-1] == '\n' {
				block = block[:len(block)-1]
			}

			// silently discard 0 length messages
			// this maintains the behavior pre 0.2.22
			if len(block) == 0 {
				continue
			}

			if int64(len(block)) > s.ctx.nsqd.getOpts().MaxMsgSize {
				return nil, http_api.Err{413, "MSG_TOO_BIG"}
			}

			msg := NewMessage(0, block)
			msgs = append(msgs, msg)
		}
	}

	err = topic.PutMessages(msgs)
	if err != nil {
		return nil, http_api.Err{503, "EXITING"}
	}

	return "OK", nil
}

func (s *httpServer) doCreateTopic(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	_, _, err := s.getTopicFromQuery(req)
	return nil, err
}

func (s *httpServer) doEmptyTopic(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	reqParams, err := http_api.NewReqParams(req)
	if err != nil {
		nsqLog.LogErrorf("failed to parse request params - %s", err)
		return nil, http_api.Err{400, "INVALID_REQUEST"}
	}

	topicName, err := reqParams.Get("topic")
	if err != nil {
		return nil, http_api.Err{400, "MISSING_ARG_TOPIC"}
	}

	if !protocol.IsValidTopicName(topicName) {
		return nil, http_api.Err{400, "INVALID_TOPIC"}
	}

	topic, err := s.ctx.nsqd.GetExistingTopic(topicName)
	if err != nil {
		return nil, http_api.Err{404, "TOPIC_NOT_FOUND"}
	}

	err = topic.empty()
	if err != nil {
		return nil, http_api.Err{500, "INTERNAL_ERROR"}
	}

	return nil, nil
}

func (s *httpServer) doDeleteTopic(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	reqParams, err := http_api.NewReqParams(req)
	if err != nil {
		nsqLog.LogErrorf("failed to parse request params - %s", err)
		return nil, http_api.Err{400, "INVALID_REQUEST"}
	}

	topicName, err := reqParams.Get("topic")
	if err != nil {
		return nil, http_api.Err{400, "MISSING_ARG_TOPIC"}
	}

	err = s.ctx.nsqd.DeleteExistingTopic(topicName)
	if err != nil {
		return nil, http_api.Err{404, "TOPIC_NOT_FOUND"}
	}

	return nil, nil
}

func (s *httpServer) doCreateChannel(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	_, topic, channelName, err := s.getExistingTopicFromQuery(req)
	if err != nil {
		return nil, err
	}
	topic.GetChannel(channelName)
	return nil, nil
}

func (s *httpServer) doDeleteChannel(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	_, topic, channelName, err := s.getExistingTopicFromQuery(req)
	if err != nil {
		return nil, err
	}

	err = topic.DeleteExistingChannel(channelName)
	if err != nil {
		return nil, http_api.Err{404, "CHANNEL_NOT_FOUND"}
	}

	return nil, nil
}

func (s *httpServer) doPauseChannel(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	_, topic, channelName, err := s.getExistingTopicFromQuery(req)
	if err != nil {
		return nil, err
	}

	channel, err := topic.GetExistingChannel(channelName)
	if err != nil {
		return nil, http_api.Err{404, "CHANNEL_NOT_FOUND"}
	}

	if strings.Contains(req.URL.Path, "unpause") {
		err = channel.UnPause()
	} else {
		err = channel.Pause()
	}
	if err != nil {
		nsqLog.LogErrorf("failure in %s - %s", req.URL.Path, err)
		return nil, http_api.Err{500, "INTERNAL_ERROR"}
	}

	// pro-actively persist metadata so in case of process failure
	// nsqd won't suddenly (un)pause a channel
	s.ctx.nsqd.Lock()
	s.ctx.nsqd.PersistMetadata()
	s.ctx.nsqd.Unlock()
	return nil, nil
}

func (s *httpServer) doMessageStats(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	reqParams, err := http_api.NewReqParams(req)
	if err != nil {
		nsqLog.LogErrorf("failed to parse request params - %s", err)
		return nil, http_api.Err{400, "INVALID_REQUEST"}
	}
	topicName, _ := reqParams.Get("topic")
	channelName, _ := reqParams.Get("channel")

	t, err := s.ctx.nsqd.GetExistingTopic(topicName)
	if err != nil {
		return nil, http_api.Err{404, "Topic not found"}
	}
	statStr := t.GetTopicChannelStat(channelName)

	return statStr, nil
}

func (s *httpServer) doStats(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	reqParams, err := http_api.NewReqParams(req)
	if err != nil {
		nsqLog.LogErrorf("failed to parse request params - %s", err)
		return nil, http_api.Err{400, "INVALID_REQUEST"}
	}
	formatString, _ := reqParams.Get("format")
	topicName, _ := reqParams.Get("topic")
	channelName, _ := reqParams.Get("channel")
	jsonFormat := formatString == "json"

	stats := s.ctx.nsqd.GetStats()
	health := s.ctx.nsqd.GetHealth()
	startTime := s.ctx.nsqd.GetStartTime()
	uptime := time.Since(startTime)

	// If we WERE given a topic-name, remove stats for all the other topics:
	if len(topicName) > 0 {
		// Find the desired-topic-index:
		for _, topicStats := range stats {
			if topicStats.TopicName == topicName {
				// If we WERE given a channel-name, remove stats for all the other channels:
				if len(channelName) > 0 {
					// Find the desired-channel:
					for _, channelStats := range topicStats.Channels {
						if channelStats.ChannelName == channelName {
							topicStats.Channels = []ChannelStats{channelStats}
							// We've got the channel we were looking for:
							break
						}
					}
				}

				// We've got the topic we were looking for:
				stats = []TopicStats{topicStats}
				break
			}
		}
	}

	if !jsonFormat {
		return s.printStats(stats, health, startTime, uptime), nil
	}

	return struct {
		Version   string       `json:"version"`
		Health    string       `json:"health"`
		StartTime int64        `json:"start_time"`
		Topics    []TopicStats `json:"topics"`
	}{version.Binary, health, startTime.Unix(), stats}, nil
}

func (s *httpServer) printStats(stats []TopicStats, health string, startTime time.Time, uptime time.Duration) []byte {
	var buf bytes.Buffer
	w := &buf
	now := time.Now()
	io.WriteString(w, fmt.Sprintf("%s\n", version.String("nsqd")))
	io.WriteString(w, fmt.Sprintf("start_time %v\n", startTime.Format(time.RFC3339)))
	io.WriteString(w, fmt.Sprintf("uptime %s\n", uptime))
	if len(stats) == 0 {
		io.WriteString(w, "\nNO_TOPICS\n")
		return buf.Bytes()
	}
	io.WriteString(w, fmt.Sprintf("\nHealth: %s\n", health))
	for _, t := range stats {
		var pausedPrefix string
		pausedPrefix = "   "
		io.WriteString(w, fmt.Sprintf("\n%s[%-15s] depth: %-5d be-depth: %-5d msgs: %-8d e2e%%: %s\n",
			pausedPrefix,
			t.TopicName,
			t.Depth,
			t.BackendDepth,
			t.MessageCount,
			t.E2eProcessingLatency))
		for _, c := range t.Channels {
			if c.Paused {
				pausedPrefix = "   *P "
			} else {
				pausedPrefix = "      "
			}
			io.WriteString(w,
				fmt.Sprintf("%s[%-25s] depth: %-5d be-depth: %-5d inflt: %-4d def: %-4d re-q: %-5d timeout: %-5d msgs: %-8d e2e%%: %s\n",
					pausedPrefix,
					c.ChannelName,
					c.Depth,
					c.BackendDepth,
					c.InFlightCount,
					c.DeferredCount,
					c.RequeueCount,
					c.TimeoutCount,
					c.MessageCount,
					c.E2eProcessingLatency))
			for _, client := range c.Clients {
				connectTime := time.Unix(client.ConnectTime, 0)
				// truncate to the second
				duration := time.Duration(int64(now.Sub(connectTime).Seconds())) * time.Second
				_, port, _ := net.SplitHostPort(client.RemoteAddress)
				io.WriteString(w, fmt.Sprintf("        [%s %-21s] state: %d inflt: %-4d rdy: %-4d fin: %-8d re-q: %-8d msgs: %-8d connected: %s\n",
					client.Version,
					fmt.Sprintf("%s:%s", client.Name, port),
					client.State,
					client.InFlightCount,
					client.ReadyCount,
					client.FinishCount,
					client.RequeueCount,
					client.MessageCount,
					duration,
				))
			}
		}
	}
	return buf.Bytes()
}

func (s *httpServer) doConfig(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	opt := ps.ByName("opt")

	if req.Method == "PUT" {
		// add 1 so that it's greater than our max when we test for it
		// (LimitReader returns a "fake" EOF)
		readMax := s.ctx.nsqd.getOpts().MaxMsgSize + 1
		body, err := ioutil.ReadAll(io.LimitReader(req.Body, readMax))
		if err != nil {
			return nil, http_api.Err{500, "INTERNAL_ERROR"}
		}
		if int64(len(body)) == readMax || len(body) == 0 {
			return nil, http_api.Err{413, "INVALID_VALUE"}
		}

		opts := *s.ctx.nsqd.getOpts()
		switch opt {
		case "nsqlookupd_tcp_addresses":
			err := json.Unmarshal(body, &opts.NSQLookupdTCPAddresses)
			if err != nil {
				return nil, http_api.Err{400, "INVALID_VALUE"}
			}
		case "verbose":
			err := json.Unmarshal(body, &opts.Verbose)
			if err != nil {
				return nil, http_api.Err{400, "INVALID_VALUE"}
			}
		case "log_level":
			err := json.Unmarshal(body, &opts.LogLevel)
			if err != nil {
				return nil, http_api.Err{400, "INVALID_VALUE"}
			}
			nsqLog.Logf("nsqd log level set to : %v", opts.LogLevel)
		default:
			return nil, http_api.Err{400, "INVALID_OPTION"}
		}
		s.ctx.nsqd.swapOpts(&opts)
		s.ctx.nsqd.triggerOptsNotification()
	}

	v, ok := getOptByCfgName(s.ctx.nsqd.getOpts(), opt)
	if !ok {
		return nil, http_api.Err{400, "INVALID_OPTION"}
	}

	return v, nil
}

func getOptByCfgName(opts interface{}, name string) (interface{}, bool) {
	val := reflect.ValueOf(opts).Elem()
	typ := val.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		flagName := field.Tag.Get("flag")
		cfgName := field.Tag.Get("cfg")
		if flagName == "" {
			continue
		}
		if cfgName == "" {
			cfgName = strings.Replace(flagName, "-", "_", -1)
		}
		if name != cfgName {
			continue
		}
		return val.FieldByName(field.Name).Interface(), true
	}
	return nil, false
}
