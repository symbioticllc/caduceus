// Copyright 2024 Dragonboat Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package gateway provides a grpc-gateway HTTP/JSON reverse proxy for the
// DragonboatService gRPC server. Each gRPC method is automatically available
// as a REST endpoint as defined by the HTTP annotations in dragonboat.proto.
package gateway

import (
	"context"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	dbpb "github.com/lni/dragonboat/v4/server/proto"
)

// NewHandler creates an http.Handler that proxies REST/JSON requests to the
// gRPC server listening at grpcAddr (e.g. "localhost:8080").
//
// The returned handler can be mounted on any net/http ServeMux or used directly
// as the handler for an http.Server.
//
// Example:
//
//	h, err := gateway.NewHandler(ctx, "localhost:8080")
//	http.ListenAndServe(":8081", h)
func NewHandler(ctx context.Context, grpcAddr string) (http.Handler, error) {
	mux := runtime.NewServeMux(
		// Return errors as JSON with a top-level "error" key.
		runtime.WithErrorHandler(runtime.DefaultHTTPErrorHandler),
		// Use camelCase JSON field names that match the proto field options.
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{}),
	)

	opts := []grpc.DialOption{
		// No TLS for the loopback gRPC connection between gateway and server.
		// If both servers run in-process you can replace this with an in-memory
		// credential or add mTLS here.
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	if err := dbpb.RegisterDragonboatServiceHandlerFromEndpoint(ctx, mux, grpcAddr, opts); err != nil {
		return nil, err
	}
	return mux, nil
}
