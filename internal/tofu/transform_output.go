// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"context"
	"log"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
)

// OutputTransformer is a GraphTransformer that adds all the outputs
// in the configuration to the graph.
//
// This is done for the apply graph builder even if dependent nodes
// aren't changing since there is no downside: the state will be available
// even if the dependent items aren't changing.
type OutputTransformer struct {
	Config *configs.Config

	// Refresh-only mode means that any failing output preconditions are
	// reported as warnings rather than errors
	RefreshOnly bool

	// Planning must be set to true only when we're building a planning graph.
	// It must be set to false whenever we're building an apply graph.
	Planning bool

	// If this is a planned destroy, root outputs are still in the configuration
	// so we need to record that we wish to remove them.
	Destroying bool
}

func (t *OutputTransformer) Transform(_ context.Context, g *Graph) error {
	return t.transform(g, t.Config)
}

func (t *OutputTransformer) transform(g *Graph, c *configs.Config) error {
	// If we have no config then there can be no outputs.
	if c == nil {
		return nil
	}

	// Transform all the children. We must do this first because
	// we can reference module outputs and they must show up in the
	// reference map.
	for _, cc := range c.Children {
		if err := t.transform(g, cc); err != nil {
			return err
		}
	}

	for _, o := range c.Module.Outputs {
		addr := addrs.OutputValue{Name: o.Name}

		node := &nodeExpandOutput{
			Addr:        addr,
			Module:      c.Path,
			Config:      o,
			Destroying:  t.Destroying,
			RefreshOnly: t.RefreshOnly,
			Planning:    t.Planning,
		}

		log.Printf("[TRACE] OutputTransformer: adding %s as %T", o.Name, node)
		g.Add(node)
	}

	return nil
}
