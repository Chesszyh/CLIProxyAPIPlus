package main

import (
	"fmt"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/cursor/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func main() {
	md := cursorproto.Msg("ExecClientMessage")
	if md == nil {
		panic("ExecClientMessage descriptor not found")
	}

	// Try different field names
	names := []string{
		"mcp_result", "mcpResult", "McpResult", "MCP_RESULT",
		"shell_result", "shellResult",
	}

	for _, name := range names {
		fd := md.Fields().ByName(protoreflect.Name(name))
		if fd != nil {
			fmt.Printf("Found field %q: number=%d, kind=%s\n", name, fd.Number(), fd.Kind())
		} else {
			fmt.Printf("Field %q NOT FOUND\n", name)
		}
	}

	// List all fields
	fmt.Println("\nAll fields in ExecClientMessage:")
	for i := 0; i < md.Fields().Len(); i++ {
		f := md.Fields().Get(i)
		fmt.Printf("  %d: %q (number=%d)\n", i, f.Name(), f.Number())
	}
}
