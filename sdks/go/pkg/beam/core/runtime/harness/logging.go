// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package harness

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/apache/beam/sdks/go/pkg/beam/log"
	pb "github.com/apache/beam/sdks/go/pkg/beam/model/fnexecution_v1"
	"github.com/golang/protobuf/ptypes"
)

// TODO(herohde) 10/12/2017: make this file a separate package. Then
// populate InstructionReference and PrimitiveTransformReference properly.

// TODO(herohde) 10/13/2017: add top-level harness.Main panic handler that flushes logs.
// Also make logger flush on Fatal severity messages.
type contextKey string

const instKey contextKey = "beam:inst"

func setInstID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, instKey, id)
}

func tryGetInstID(ctx context.Context) (string, bool) {
	id := ctx.Value(instKey)
	if id == nil {
		return "", false
	}
	return id.(string), true
}

type logger struct {
	out chan<- *pb.LogEntry
}

func (l *logger) Log(ctx context.Context, sev log.Severity, calldepth int, msg string) {
	now, _ := ptypes.TimestampProto(time.Now())

	entry := &pb.LogEntry{
		Timestamp: now,
		Severity:  convertSeverity(sev),
		Message:   msg,
	}
	if _, file, line, ok := runtime.Caller(calldepth); ok {
		entry.LogLocation = fmt.Sprintf("%v:%v", file, line)
	}
	if id, ok := tryGetInstID(ctx); ok {
		entry.InstructionReference = id
	}

	select {
	case l.out <- entry:
		// ok
	default:
		// buffer full: drop to stderr.
		fmt.Fprintln(os.Stderr, msg)
	}
}

func convertSeverity(sev log.Severity) pb.LogEntry_Severity_Enum {
	switch sev {
	case log.SevDebug:
		return pb.LogEntry_Severity_DEBUG
	case log.SevInfo:
		return pb.LogEntry_Severity_INFO
	case log.SevWarn:
		return pb.LogEntry_Severity_WARN
	case log.SevError:
		return pb.LogEntry_Severity_ERROR
	case log.SevFatal:
		return pb.LogEntry_Severity_CRITICAL
	default:
		return pb.LogEntry_Severity_INFO
	}
}

// setupRemoteLogging redirects local log messages to FnHarness. It will
// try to reconnect, if a connection goes bad. Falls back to stdout.
func setupRemoteLogging(ctx context.Context, endpoint string) {
	buf := make(chan *pb.LogEntry, 2000)
	log.SetLogger(&logger{out: buf})

	w := &remoteWriter{buf, endpoint}
	go w.Run(ctx)
}

type remoteWriter struct {
	buffer   chan *pb.LogEntry
	endpoint string
}

func (w *remoteWriter) Run(ctx context.Context) error {
	for {
		err := w.connect(ctx)

		fmt.Fprintf(os.Stderr, "Remote logging failed: %v. Retrying in 5 sec ...\n", err)
		time.Sleep(5 * time.Second)
	}
}

func (w *remoteWriter) connect(ctx context.Context) error {
	conn, err := dial(ctx, w.endpoint, 30*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	client, err := pb.NewBeamFnLoggingClient(conn).Logging(ctx)
	if err != nil {
		return err
	}
	defer client.CloseSend()

	for msg := range w.buffer {
		// fmt.Fprintf(os.Stderr, "REMOTE: %v\n", proto.MarshalTextString(msg))

		// TODO: batch up log messages

		list := &pb.LogEntry_List{
			LogEntries: []*pb.LogEntry{msg},
		}

		recordLogEntries(list)

		if err := client.Send(list); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to send message: %v\n%v", err, msg)
			return err
		}

		// fmt.Fprintf(os.Stderr, "SENT: %v\n", msg)
	}
	return fmt.Errorf("internal: buffer closed?")
}
