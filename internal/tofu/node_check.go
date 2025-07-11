// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"context"
	"log"

	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/checks"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/dag"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

var (
	_ GraphNodeModulePath = (*nodeReportCheck)(nil)
	_ GraphNodeExecutable = (*nodeReportCheck)(nil)
)

// nodeReportCheck calls the ReportCheckableObjects function for our assertions
// within the check blocks.
//
// We need this to happen before the checks are actually verified and before any
// nested data blocks, so the creator of this structure should make sure this
// node is a parent of any nested data blocks.
//
// This needs to be separate to nodeExpandCheck, because the actual checks
// should happen after referenced data blocks rather than before.
type nodeReportCheck struct {
	addr addrs.ConfigCheck
}

func (n *nodeReportCheck) ModulePath() addrs.Module {
	return n.addr.Module
}

func (n *nodeReportCheck) Execute(_ context.Context, evalCtx EvalContext, _ walkOperation) tfdiags.Diagnostics {
	exp := evalCtx.InstanceExpander()
	modInsts := exp.ExpandModule(n.ModulePath())

	instAddrs := addrs.MakeSet[addrs.Checkable]()
	for _, modAddr := range modInsts {
		instAddrs.Add(n.addr.Check.Absolute(modAddr))
	}
	evalCtx.Checks().ReportCheckableObjects(n.addr, instAddrs)
	return nil
}

func (n *nodeReportCheck) Name() string {
	return n.addr.String() + " (report)"
}

var (
	_ GraphNodeModulePath        = (*nodeExpandCheck)(nil)
	_ GraphNodeDynamicExpandable = (*nodeExpandCheck)(nil)
	_ GraphNodeReferencer        = (*nodeExpandCheck)(nil)
)

// nodeExpandCheck creates child nodes that actually execute the assertions for
// a given check block.
//
// This must happen after any other nodes/resources/data sources that are
// referenced, so we implement GraphNodeReferencer.
//
// This needs to be separate to nodeReportCheck as nodeReportCheck must happen
// first, while nodeExpandCheck must execute after any referenced blocks.
type nodeExpandCheck struct {
	addr   addrs.ConfigCheck
	config *configs.Check

	makeInstance func(addrs.AbsCheck, *configs.Check) dag.Vertex
}

func (n *nodeExpandCheck) ModulePath() addrs.Module {
	return n.addr.Module
}

func (n *nodeExpandCheck) DynamicExpand(ctx EvalContext) (*Graph, error) {
	exp := ctx.InstanceExpander()
	modInsts := exp.ExpandModule(n.ModulePath())

	var g Graph
	for _, modAddr := range modInsts {
		testAddr := n.addr.Check.Absolute(modAddr)
		log.Printf("[TRACE] nodeExpandCheck: Node for %s", testAddr)
		g.Add(n.makeInstance(testAddr, n.config))
	}
	addRootNodeToGraph(&g)

	return &g, nil
}

func (n *nodeExpandCheck) References() []*addrs.Reference {
	var refs []*addrs.Reference
	for _, assert := range n.config.Asserts {
		// Check blocks reference anything referenced by conditions or messages
		// in their check rules.
		condition, _ := lang.ReferencesInExpr(addrs.ParseRef, assert.Condition)
		message, _ := lang.ReferencesInExpr(addrs.ParseRef, assert.ErrorMessage)
		refs = append(refs, condition...)
		refs = append(refs, message...)
	}
	if n.config.DataResource != nil {
		// We'll also always reference our nested data block if it exists, as
		// there is nothing enforcing that it has to also be referenced by our
		// conditions or messages.
		//
		// We don't need to make this addr absolute, because the check block and
		// the data resource are always within the same module/instance.
		traversal, _ := hclsyntax.ParseTraversalAbs(
			[]byte(n.config.DataResource.Addr().String()),
			n.config.DataResource.DeclRange.Filename,
			n.config.DataResource.DeclRange.Start)
		ref, _ := addrs.ParseRef(traversal)
		refs = append(refs, ref)
	}
	return refs
}

func (n *nodeExpandCheck) Name() string {
	return n.addr.String() + " (expand)"
}

var (
	_ GraphNodeModuleInstance = (*nodeCheckAssert)(nil)
	_ GraphNodeExecutable     = (*nodeCheckAssert)(nil)
)

type nodeCheckAssert struct {
	addr   addrs.AbsCheck
	config *configs.Check

	// We only want to actually execute the checks during the plan and apply
	// operations, but we still want to validate our config during
	// other operations.
	executeChecks bool
}

func (n *nodeCheckAssert) ModulePath() addrs.Module {
	return n.Path().Module()
}

func (n *nodeCheckAssert) Path() addrs.ModuleInstance {
	return n.addr.Module
}

func (n *nodeCheckAssert) Execute(ctx context.Context, evalCtx EvalContext, _ walkOperation) tfdiags.Diagnostics {

	// We only want to actually execute the checks during specific
	// operations, such as plan and applies.
	if n.executeChecks {
		if status := evalCtx.Checks().ObjectCheckStatus(n.addr); status == checks.StatusFail || status == checks.StatusError {
			// This check is already failing, so we won't try and evaluate it.
			// This typically means there was an error in a data block within
			// the check block.
			return nil
		}

		return evalCheckRules(
			ctx,
			addrs.CheckAssertion,
			n.config.Asserts,
			evalCtx,
			n.addr,
			EvalDataForNoInstanceKey,
			tfdiags.Warning)

	}

	// Otherwise let's still validate the config and references and return
	// diagnostics if references do not exist etc.
	var diags tfdiags.Diagnostics
	for ix, assert := range n.config.Asserts {
		_, _, moreDiags := validateCheckRule(ctx, addrs.NewCheckRule(n.addr, addrs.CheckAssertion, ix), assert, evalCtx, EvalDataForNoInstanceKey)
		diags = diags.Append(moreDiags)
	}
	return diags
}

func (n *nodeCheckAssert) Name() string {
	return n.addr.String() + " (assertions)"
}

var (
	_ GraphNodeExecutable = (*nodeCheckStart)(nil)
)

// We need to ensure that any nested data sources execute after all other
// resource changes have been applied. This node acts as a single point of
// dependency that can enforce this ordering.
type nodeCheckStart struct{}

func (n *nodeCheckStart) Execute(_ context.Context, _ EvalContext, _ walkOperation) tfdiags.Diagnostics {
	// This node doesn't actually do anything, except simplify the underlying
	// graph structure.
	return nil
}

func (n *nodeCheckStart) Name() string {
	return "(execute checks)"
}
