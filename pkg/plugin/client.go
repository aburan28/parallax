/*
Copyright 2026 The Parallax Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package plugin

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Dial connects to a plugin over its unix socket. Used by the host after it has read
// and parsed the plugin's handshake line. The connection is plaintext because it is a
// host-local unix socket inside the operator pod (DESIGN.md §5.5).
func Dial(ctx context.Context, h Handshake) (*grpc.ClientConn, error) {
	if h.Network != "unix" {
		return nil, fmt.Errorf("unsupported plugin network %q", h.Network)
	}
	conn, err := grpc.NewClient(
		"unix:"+h.Address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", h.Address)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("dial plugin socket %s: %w", h.Address, err)
	}
	return conn, nil
}
