// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package xform

import (
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/ordering"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/props"
	"github.com/cockroachdb/errors"
)

// IsCanonicalGroupBy returns true if the private is for the canonical version
// of the grouping operator. This is the operator that is built initially (and
// has all grouping columns as optional in the ordering), as opposed to variants
// generated by the GenerateStreamingGroupBy exploration rule.
func (c *CustomFuncs) IsCanonicalGroupBy(private *memo.GroupingPrivate) bool {
	return private.Ordering.Any() || private.GroupingCols.SubsetOf(private.Ordering.Optional)
}

// MakeProjectFromPassthroughAggs constructs a top-level Project operator that
// contains one output column per function in the given aggregrate list. The
// input expression is expected to return zero or one rows, and the aggregate
// functions are expected to always pass through their values in that case.
func (c *CustomFuncs) MakeProjectFromPassthroughAggs(
	grp memo.RelExpr, input memo.RelExpr, aggs memo.AggregationsExpr,
) {
	if !input.Relational().Cardinality.IsZeroOrOne() {
		panic(errors.AssertionFailedf("input expression cannot have more than one row: %v", input))
	}

	var passthrough opt.ColSet
	projections := make(memo.ProjectionsExpr, 0, len(aggs))
	for i := range aggs {
		// If aggregate remaps the column ID, need to synthesize projection item;
		// otherwise, can just pass through.
		variable := aggs[i].Agg.Child(0).(*memo.VariableExpr)
		if variable.Col == aggs[i].Col {
			passthrough.Add(variable.Col)
		} else {
			projections = append(projections, c.e.f.ConstructProjectionsItem(variable, aggs[i].Col))
		}
	}
	c.e.mem.AddProjectToGroup(&memo.ProjectExpr{
		Input:       input,
		Projections: projections,
		Passthrough: passthrough,
	}, grp)
}

// GenerateStreamingGroupBy generates variants of a GroupBy, DistinctOn,
// EnsureDistinctOn, UpsertDistinctOn, or EnsureUpsertDistinctOn expression
// with more specific orderings on the grouping columns, using the interesting
// orderings property. See the GenerateStreamingGroupBy rule.
func (c *CustomFuncs) GenerateStreamingGroupBy(
	grp memo.RelExpr,
	op opt.Operator,
	input memo.RelExpr,
	aggs memo.AggregationsExpr,
	private *memo.GroupingPrivate,
) {
	orders := ordering.DeriveInterestingOrderings(input)
	intraOrd := private.Ordering
	for _, ord := range orders {
		o := ord.ToOrdering()
		// We are looking for a prefix of o that satisfies the intra-group ordering
		// if we ignore grouping columns.
		oIdx, intraIdx := 0, 0
		for ; oIdx < len(o); oIdx++ {
			oCol := o[oIdx].ID()
			if private.GroupingCols.Contains(oCol) || intraOrd.Optional.Contains(oCol) {
				// Grouping or optional column.
				continue
			}

			if intraIdx < len(intraOrd.Columns) &&
				intraOrd.Columns[intraIdx].Group.Contains(oCol) &&
				intraOrd.Columns[intraIdx].Descending == o[oIdx].Descending() {
				// Column matches the one in the ordering.
				intraIdx++
				continue
			}
			break
		}
		if oIdx == 0 || intraIdx < len(intraOrd.Columns) {
			// No match.
			continue
		}
		o = o[:oIdx]

		var newOrd props.OrderingChoice
		newOrd.FromOrderingWithOptCols(o, opt.ColSet{})

		// Simplify the ordering according to the input's FDs. Note that this is not
		// necessary for correctness because buildChildPhysicalProps would do it
		// anyway, but doing it here once can make things more efficient (and we may
		// generate fewer expressions if some of these orderings turn out to be
		// equivalent).
		newOrd.Simplify(&input.Relational().FuncDeps)

		newPrivate := *private
		newPrivate.Ordering = newOrd

		switch op {
		case opt.GroupByOp:
			newExpr := memo.GroupByExpr{
				Input:           input,
				Aggregations:    aggs,
				GroupingPrivate: newPrivate,
			}
			c.e.mem.AddGroupByToGroup(&newExpr, grp)

		case opt.DistinctOnOp:
			newExpr := memo.DistinctOnExpr{
				Input:           input,
				Aggregations:    aggs,
				GroupingPrivate: newPrivate,
			}
			c.e.mem.AddDistinctOnToGroup(&newExpr, grp)

		case opt.EnsureDistinctOnOp:
			newExpr := memo.EnsureDistinctOnExpr{
				Input:           input,
				Aggregations:    aggs,
				GroupingPrivate: newPrivate,
			}
			c.e.mem.AddEnsureDistinctOnToGroup(&newExpr, grp)

		case opt.UpsertDistinctOnOp:
			newExpr := memo.UpsertDistinctOnExpr{
				Input:           input,
				Aggregations:    aggs,
				GroupingPrivate: newPrivate,
			}
			c.e.mem.AddUpsertDistinctOnToGroup(&newExpr, grp)

		case opt.EnsureUpsertDistinctOnOp:
			newExpr := memo.EnsureUpsertDistinctOnExpr{
				Input:           input,
				Aggregations:    aggs,
				GroupingPrivate: newPrivate,
			}
			c.e.mem.AddEnsureUpsertDistinctOnToGroup(&newExpr, grp)
		}
	}
}

// OtherAggsAreConst returns true if all items in the given aggregate list
// contain ConstAgg functions, except for the "except" item. The ConstAgg
// functions will always return the same value, as long as there is at least
// one input row.
func (c *CustomFuncs) OtherAggsAreConst(
	aggs memo.AggregationsExpr, except *memo.AggregationsItem,
) bool {
	for i := range aggs {
		agg := &aggs[i]
		if agg == except {
			continue
		}

		switch agg.Agg.Op() {
		case opt.ConstAggOp:
			// Ensure that argument is a VariableOp.
			if agg.Agg.Child(0).Op() != opt.VariableOp {
				return false
			}

		default:
			return false
		}
	}
	return true
}

// MakeOrderingChoiceFromColumn constructs a new OrderingChoice with
// one element in the sequence: the columnID in the order defined by
// (MIN/MAX) operator. This function was originally created to be used
// with the Replace(Min|Max)WithLimit exploration rules.
//
// WARNING: The MinOp case can return a NULL value if the column allows it. This
// is because NULL values sort first in CRDB.
func (c *CustomFuncs) MakeOrderingChoiceFromColumn(
	op opt.Operator, col opt.ColumnID,
) props.OrderingChoice {
	oc := props.OrderingChoice{}
	switch op {
	case opt.MinOp:
		oc.AppendCol(col, false /* descending */)
	case opt.MaxOp:
		oc.AppendCol(col, true /* descending */)
	}
	return oc
}

// SplitGroupByScanIntoUnionScans splits a non-inverted scan under a GroupBy,
// DistinctOn, or EnsureUpsertDistinctOn into a UnionAll of scans, where each
// scan can provide an ordering on the grouping columns. If no such UnionAll
// can be built, returns ok=false.
//
// This is useful because the GenerateStreamingGroupBy rule can then create a
// streaming grouping operation, which is more efficient.
// GenerateStreamingGroupBy will use the new interesting orderings provided by
// the UnionAll of scans to build the streaming operation.
//
// See the SplitGroupByScanIntoUnionScans rule for more details.
func (c *CustomFuncs) SplitGroupByScanIntoUnionScans(
	scan memo.RelExpr, sp *memo.ScanPrivate, private *memo.GroupingPrivate,
) (_ memo.RelExpr, ok bool) {
	cons, ok := c.getKnownScanConstraint(sp)
	if !ok {
		// No valid constraint was found.
		return nil, false
	}

	intraOrd := private.Ordering

	// Find the length of the prefix of index columns preceding the first groupby
	// ordering column. We will verify later that the entire ordering sequence is
	// represented in the index. Ex:
	//
	//     Index: +1/+2/-3, Group By internal ordering +3 opt(4) => Prefix Length: 2
	//
	keyPrefixLength := cons.Columns.Count()
	for i := 0; i < cons.Columns.Count(); i++ {
		col := cons.Columns.Get(i).ID()
		if private.GroupingCols.Contains(col) || intraOrd.Optional.Contains(col) {
			// Grouping or optional column.
			keyPrefixLength = i
			break
		}
		if len(intraOrd.Columns) > 0 &&
			intraOrd.Columns[0].Group.Contains(col) {
			// Column matches the one in the ordering.
			keyPrefixLength = i
			break
		}
	}
	if keyPrefixLength == 0 {
		// This case can be handled by GenerateStreamingGroupBy.
		return nil, false
	}

	// Create a UnionAll of scans that can provide the ordering of the
	// GroupingPrivate (if no such UnionAll is possible this will return
	// ok=false). We pass a limit of 0 since the scans are unlimited
	// (splitScanIntoUnionScans is also used for another rule with limited scans).
	return c.splitScanIntoUnionScans(
		intraOrd, scan, sp, cons, 0 /* limit */, keyPrefixLength,
	)
}

// GroupingColumns returns the grouping columns from the grouping private.
func (c *CustomFuncs) GroupingColumns(private *memo.GroupingPrivate) opt.ColSet {
	return private.GroupingCols
}

// GroupingOrdering returns the ordering from the grouping private.
func (c *CustomFuncs) GroupingOrdering(private *memo.GroupingPrivate) props.OrderingChoice {
	return private.Ordering
}
