// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package plan

import (
	"fmt"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/util/types"
)

// UseNewPlanner means if use the new planner.
var UseNewPlanner = false

func (b *planBuilder) allocID(p Plan) string {
	b.id++
	return fmt.Sprintf("%T_%d", p, b.id)
}

func (b *planBuilder) buildAggregation(p Plan, aggFuncList []*ast.AggregateFuncExpr, gby []expression.Expression, correlated bool) Plan {
	newAggFuncList := make([]expression.AggregationFunction, 0, len(aggFuncList))
	agg := &Aggregation{}
	agg.id = b.allocID(agg)
	agg.correlated = p.IsCorrelated() || correlated
	addChild(agg, p)
	schema := make([]*expression.Column, 0, len(aggFuncList))
	for i, aggFunc := range aggFuncList {
		var newArgList []expression.Expression
		for _, arg := range aggFunc.Args {
			newArg, np, correlated, err := b.rewrite(arg, p, nil)
			if err != nil {
				b.err = errors.Trace(err)
				return nil
			}
			p = np
			agg.correlated = correlated || agg.correlated
			newArgList = append(newArgList, newArg)
		}
		newAggFuncList = append(newAggFuncList, expression.NewAggFunction(aggFunc.F, newArgList, aggFunc.Distinct))
		schema = append(schema, &expression.Column{FromID: agg.id,
			ColName: model.NewCIStr(fmt.Sprintf("%s_col_%d", agg.id, i))})
	}
	agg.AggFuncs = newAggFuncList
	agg.GroupByItems = gby
	agg.SetSchema(schema)
	return agg
}

func (b *planBuilder) buildResultSetNode(node ast.ResultSetNode) Plan {
	switch x := node.(type) {
	case *ast.Join:
		return b.buildNewJoin(x)
	case *ast.TableSource:
		var p Plan
		switch v := x.Source.(type) {
		case *ast.SelectStmt:
			p = b.buildNewSelect(v)
		case *ast.UnionStmt:
			p = b.buildNewUnion(v)
		case *ast.TableName:
			// TODO: select physical algorithm during cbo phase.
			p = b.buildNewTableScanPlan(v)
		default:
			b.err = ErrUnsupportedType.Gen("unsupported table source type %T", v)
			return nil
		}
		if b.err != nil {
			return nil
		}
		if v, ok := p.(*NewTableScan); ok {
			v.TableAsName = &x.AsName
		}
		if x.AsName.L != "" {
			schema := p.GetSchema()
			for _, col := range schema {
				col.TblName = x.AsName
				col.DBName = model.NewCIStr("")
			}
		}
		return p
	case *ast.SelectStmt:
		return b.buildNewSelect(x)
	case *ast.UnionStmt:
		return b.buildNewUnion(x)
	default:
		b.err = ErrUnsupportedType.Gen("unsupported table source type %T", x)
		return nil
	}
}

func extractColumn(expr expression.Expression, cols []*expression.Column, outerCols []*expression.Column) (result []*expression.Column, outer []*expression.Column) {
	switch v := expr.(type) {
	case *expression.Column:
		if v.Correlated {
			return cols, append(outerCols, v)
		}
		return append(cols, v), outerCols
	case *expression.ScalarFunction:
		for _, arg := range v.Args {
			cols, outerCols = extractColumn(arg, cols, outerCols)
		}
		return cols, outerCols
	}
	return cols, outerCols
}

func extractOnCondition(conditions []expression.Expression, left Plan, right Plan) (
	eqCond []*expression.ScalarFunction, leftCond []expression.Expression, rightCond []expression.Expression,
	otherCond []expression.Expression) {
	for _, expr := range conditions {
		binop, ok := expr.(*expression.ScalarFunction)
		if ok && binop.FuncName.L == ast.EQ {
			ln, lOK := binop.Args[0].(*expression.Column)
			rn, rOK := binop.Args[1].(*expression.Column)
			if lOK && rOK {
				if left.GetSchema().GetIndex(ln) != -1 && right.GetSchema().GetIndex(rn) != -1 {
					eqCond = append(eqCond, binop)
					continue
				}
				if left.GetSchema().GetIndex(rn) != -1 && right.GetSchema().GetIndex(ln) != -1 {
					newEq := expression.NewFunction(model.NewCIStr(ast.EQ), []expression.Expression{rn, ln})
					eqCond = append(eqCond, newEq)
					continue
				}
			}
		}
		columns, _ := extractColumn(expr, nil, nil)
		allFromLeft, allFromRight := true, true
		for _, col := range columns {
			if left.GetSchema().GetIndex(col) != -1 {
				allFromRight = false
			} else {
				allFromLeft = false
			}
		}
		if allFromRight {
			rightCond = append(rightCond, expr)
		} else if allFromLeft {
			leftCond = append(leftCond, expr)
		} else {
			otherCond = append(otherCond, expr)
		}
	}
	return
}

// CNF means conjunctive normal form, e.g. a and b and c.
func splitCNFItems(onExpr expression.Expression) []expression.Expression {
	switch v := onExpr.(type) {
	case *expression.ScalarFunction:
		if v.FuncName.L == ast.AndAnd {
			var ret []expression.Expression
			for _, arg := range v.Args {
				ret = append(ret, splitCNFItems(arg)...)
			}
			return ret
		}
	}
	return []expression.Expression{onExpr}
}

func (b *planBuilder) buildNewJoin(join *ast.Join) Plan {
	if join.Right == nil {
		return b.buildResultSetNode(join.Left)
	}
	leftPlan := b.buildResultSetNode(join.Left)
	rightPlan := b.buildResultSetNode(join.Right)
	newSchema := append(leftPlan.GetSchema().DeepCopy(), rightPlan.GetSchema().DeepCopy()...)
	joinPlan := &Join{}
	joinPlan.SetSchema(newSchema)
	joinPlan.correlated = leftPlan.IsCorrelated() || rightPlan.IsCorrelated()
	if join.On != nil {
		onExpr, _, correlated, err := b.rewrite(join.On.Expr, joinPlan, nil)
		if err != nil {
			b.err = err
			return nil
		}
		if correlated {
			b.err = errors.New("On condition doesn't support subqueries yet.")
		}
		onCondition := splitCNFItems(onExpr)
		eqCond, leftCond, rightCond, otherCond := extractOnCondition(onCondition, leftPlan, rightPlan)
		joinPlan.EqualConditions = eqCond
		joinPlan.LeftConditions = leftCond
		joinPlan.RightConditions = rightCond
		joinPlan.OtherConditions = otherCond
	}
	if join.Tp == ast.LeftJoin {
		joinPlan.JoinType = LeftOuterJoin
	} else if join.Tp == ast.RightJoin {
		joinPlan.JoinType = RightOuterJoin
	} else {
		joinPlan.JoinType = InnerJoin
	}
	addChild(joinPlan, leftPlan)
	addChild(joinPlan, rightPlan)
	return joinPlan
}

func (b *planBuilder) buildSelection(p Plan, where ast.ExprNode, mapper map[*ast.AggregateFuncExpr]int) Plan {
	conditions := splitWhere(where)
	expressions := make([]expression.Expression, 0, len(conditions))
	selection := &Selection{}
	selection.correlated = p.IsCorrelated()
	for _, cond := range conditions {
		expr, np, correlated, err := b.rewrite(cond, p, mapper)
		if err != nil {
			b.err = err
			return nil
		}
		p = np
		selection.correlated = selection.correlated || correlated
		expressions = append(expressions, expr)
	}
	selection.Conditions = expressions
	selection.id = b.allocID(selection)
	selection.SetSchema(p.GetSchema().DeepCopy())
	addChild(selection, p)
	return selection
}

func (b *planBuilder) buildProjection(p Plan, fields []*ast.SelectField, mapper map[*ast.AggregateFuncExpr]int) (Plan, int) {
	proj := &Projection{Exprs: make([]expression.Expression, 0, len(fields))}
	proj.id = b.allocID(proj)
	proj.correlated = p.IsCorrelated()
	schema := make(expression.Schema, 0, len(fields))
	oldLen := 0
	for _, field := range fields {
		var tblName, colName model.CIStr
		if field.WildCard != nil {
			dbName := field.WildCard.Schema
			colTblName := field.WildCard.Table
			for _, col := range p.GetSchema() {
				matchTable := (dbName.L == "" || dbName.L == col.DBName.L) &&
					(colTblName.L == "" || colTblName.L == col.TblName.L)
				if !matchTable {
					continue
				}
				newExpr := col.DeepCopy()
				proj.Exprs = append(proj.Exprs, newExpr)
				schemaCol := &expression.Column{
					FromID:  col.FromID,
					TblName: col.TblName,
					ColName: col.ColName,
					RetType: newExpr.GetType()}
				schema = append(schema, schemaCol)
				if !field.Auxiliary {
					oldLen++
				}
			}
		} else {
			newExpr, np, correlated, err := b.rewrite(field.Expr, p, mapper)
			if err != nil {
				b.err = errors.Trace(err)
				return nil, oldLen
			}
			if !field.Auxiliary {
				oldLen++
			}
			p = np
			proj.correlated = proj.correlated || correlated
			proj.Exprs = append(proj.Exprs, newExpr)
			var fromID string
			if field.AsName.L != "" {
				colName = field.AsName
				fromID = proj.id
			} else if c, ok := newExpr.(*expression.Column); ok {
				colName = c.ColName
				tblName = c.TblName
				fromID = c.FromID
			} else {
				colName = model.NewCIStr(field.Expr.Text())
				fromID = proj.id
			}
			schemaCol := &expression.Column{
				FromID:    fromID,
				TblName:   tblName,
				ColName:   colName,
				RetType:   newExpr.GetType(),
				Auxiliary: field.Auxiliary,
			}
			schema = append(schema, schemaCol)
		}
	}
	proj.SetSchema(schema)
	addChild(proj, p)
	return proj, oldLen
}

func (b *planBuilder) buildNewDistinct(src Plan) Plan {
	d := &Distinct{}
	addChild(d, src)
	d.SetSchema(src.GetSchema())
	return d
}

func (b *planBuilder) buildNewUnion(union *ast.UnionStmt) (p Plan) {
	sels := make([]Plan, len(union.SelectList.Selects))
	for i, sel := range union.SelectList.Selects {
		sels[i] = b.buildNewSelect(sel)
	}
	u := &Union{
		Selects: sels,
	}
	u.id = b.allocID(u)
	p = u
	firstSchema := make(expression.Schema, 0, len(sels[0].GetSchema()))
	firstSchema = append(firstSchema, sels[0].GetSchema()...)
	for _, sel := range sels {
		if len(firstSchema) != len(sel.GetSchema()) {
			b.err = errors.New("The used SELECT statements have a different number of columns")
			return nil
		}
		for i, col := range sel.GetSchema() {
			/*
			 * The lengths of the columns in the UNION result take into account the values retrieved by all of the SELECT statements
			 * SELECT REPEAT('a',1) UNION SELECT REPEAT('b',10);
			 * +---------------+
			 * | REPEAT('a',1) |
			 * +---------------+
			 * | a             |
			 * | bbbbbbbbbb    |
			 * +---------------+
			 */
			if col.RetType.Flen > firstSchema[i].RetType.Flen {
				firstSchema[i].RetType.Flen = col.RetType.Flen
			}
			// For select nul union select "abc", we should not convert "abc" to nil.
			// And the result field type should be VARCHAR.
			if firstSchema[i].RetType.Tp == 0 || firstSchema[i].RetType.Tp == mysql.TypeNull {
				firstSchema[i].RetType.Tp = col.RetType.Tp
			}
		}
		addChild(p, sel)
	}
	for _, v := range firstSchema {
		v.FromID = u.id
		v.DBName = model.NewCIStr("")
	}

	p.SetSchema(firstSchema)
	if union.Distinct {
		p = b.buildNewDistinct(p)
	}
	if union.OrderBy != nil {
		p = b.buildNewSort(p, union.OrderBy.Items, nil)
	}
	if union.Limit != nil {
		p = b.buildNewLimit(p, union.Limit)
	}
	return p
}

// ByItems wraps a "by" item.
type ByItems struct {
	Expr expression.Expression
	Desc bool
}

// NewSort stands for the order by plan.
type NewSort struct {
	basePlan

	ByItems []ByItems
}

func (b *planBuilder) buildNewSort(p Plan, byItems []*ast.ByItem, mapper map[*ast.AggregateFuncExpr]int) Plan {
	var exprs []ByItems
	sort := &NewSort{}
	for _, item := range byItems {
		it, np, correlated, err := b.rewrite(item.Expr, p, mapper)
		if err != nil {
			b.err = err
			return nil
		}
		p = np
		sort.correlated = sort.correlated || correlated
		exprs = append(exprs, ByItems{Expr: it, Desc: item.Desc})
	}
	sort.ByItems = exprs
	addChild(sort, p)
	sort.id = b.allocID(sort)
	sort.SetSchema(p.GetSchema().DeepCopy())
	return sort
}

func (b *planBuilder) buildNewLimit(src Plan, limit *ast.Limit) Plan {
	li := &Limit{
		Offset: limit.Offset,
		Count:  limit.Count,
	}
	if s, ok := src.(*Sort); ok {
		s.ExecLimit = li
		return s
	}
	addChild(li, src)
	li.SetSchema(src.GetSchema().DeepCopy())
	return li
}

func (b *planBuilder) extractAggFunc(sel *ast.SelectStmt) (
	[]*ast.AggregateFuncExpr, map[*ast.AggregateFuncExpr]int,
	map[*ast.AggregateFuncExpr]int, map[*ast.AggregateFuncExpr]int) {
	extractor := &ast.AggregateFuncExtractor{AggFuncs: make([]*ast.AggregateFuncExpr, 0)}
	// Extract agg funcs from having clause.
	if sel.Having != nil {
		n, ok := sel.Having.Expr.Accept(extractor)
		if !ok {
			b.err = errors.New("Failed to extract agg expr from having clause")
			return nil, nil, nil, nil
		}
		sel.Having.Expr = n.(ast.ExprNode)
	}
	havingAggFuncs := extractor.AggFuncs
	extractor.AggFuncs = make([]*ast.AggregateFuncExpr, 0)
	havingMapper := make(map[*ast.AggregateFuncExpr]int)
	for _, agg := range havingAggFuncs {
		havingMapper[agg] = len(sel.Fields.Fields)
		field := &ast.SelectField{Expr: agg,
			AsName:    model.NewCIStr(fmt.Sprintf("sel_agg_%d", len(sel.Fields.Fields))),
			Auxiliary: true}
		sel.Fields.Fields = append(sel.Fields.Fields, field)
	}

	// Extract agg funcs from order by clause.
	if sel.OrderBy != nil {
		for _, item := range sel.OrderBy.Items {
			_, ok := item.Expr.Accept(extractor)
			if !ok {
				b.err = errors.New("Failed to extract agg expr from orderby clause")
				return nil, nil, nil, nil
			}
		}
	}
	orderByAggFuncs := extractor.AggFuncs
	extractor.AggFuncs = make([]*ast.AggregateFuncExpr, 0)
	orderByMapper := make(map[*ast.AggregateFuncExpr]int)
	for _, agg := range orderByAggFuncs {
		orderByMapper[agg] = len(sel.Fields.Fields)
		field := &ast.SelectField{Expr: agg,
			AsName:    model.NewCIStr(fmt.Sprintf("sel_agg_%d", len(sel.Fields.Fields))),
			Auxiliary: true}
		sel.Fields.Fields = append(sel.Fields.Fields, field)
	}

	for i, f := range sel.Fields.Fields {
		n, ok := f.Expr.Accept(extractor)
		if !ok {
			b.err = errors.New("Failed to extract agg expr!")
			return nil, nil, nil, nil
		}
		expr, _ := n.(ast.ExprNode)
		sel.Fields.Fields[i].Expr = expr
	}
	aggList := extractor.AggFuncs
	aggList = append(aggList, havingAggFuncs...)
	aggList = append(aggList, orderByAggFuncs...)
	totalAggMapper := make(map[*ast.AggregateFuncExpr]int)

	for i, agg := range aggList {
		totalAggMapper[agg] = i
	}
	return aggList, havingMapper, orderByMapper, totalAggMapper
}

type astColsReplacer struct {
	sel     *Projection
	inExpr  bool
	orderBy bool
	err     error
}

func (e *astColsReplacer) addSel(selCol *expression.Column) {
	e.sel.Exprs = append(e.sel.Exprs, selCol)
	schemaCols, _ := selCol.DeepCopy().(*expression.Column)
	schemaCols.Auxiliary = true
	e.sel.schema = append(e.sel.schema, schemaCols)
}

func (e *astColsReplacer) Enter(inNode ast.Node) (ast.Node, bool) {
	switch v := inNode.(type) {
	case *ast.ValueExpr, *ast.ColumnNameExpr, *ast.ParenthesesExpr:
	case *ast.ColumnName:
		var first, second expression.Schema
		if e.orderBy && e.inExpr {
			second, first = e.sel.GetSchema(), e.sel.GetChildByIndex(0).GetSchema()
		} else {
			first, second = e.sel.GetSchema(), e.sel.GetChildByIndex(0).GetSchema()
		}
		selCol, err := first.FindColumn(v)
		if err != nil {
			e.err = errors.Trace(err)
			return inNode, true
		}
		if selCol == nil {
			selCol, err = second.FindColumn(v)
			if err != nil {
				e.err = errors.Trace(err)
				return inNode, true
			}
			if selCol == nil {
				e.err = errors.Errorf("Can't find Column %s", v.Name)
				return inNode, true
			}
			if !e.orderBy || !e.inExpr {
				e.addSel(selCol)
			}
		} else if e.orderBy && e.inExpr {
			v.Table = selCol.TblName
			e.addSel(selCol)
		}
	case *ast.SubqueryExpr, *ast.CompareSubqueryExpr, *ast.ExistsSubqueryExpr:
		return inNode, true
	default:
		e.inExpr = true
	}
	return inNode, false
}

func (e *astColsReplacer) Leave(inNode ast.Node) (ast.Node, bool) {
	return inNode, true
}

type gbyColsReplacer struct {
	fields []*ast.SelectField
	schema expression.Schema
	err    error
}

func (g *gbyColsReplacer) Enter(inNode ast.Node) (ast.Node, bool) {
	switch inNode.(type) {
	case *ast.SubqueryExpr, *ast.CompareSubqueryExpr, *ast.ExistsSubqueryExpr:
		return inNode, true
	}
	return inNode, false
}

func (g *gbyColsReplacer) Leave(inNode ast.Node) (ast.Node, bool) {
	switch v := inNode.(type) {
	case *ast.ColumnNameExpr:
		if col, err := g.schema.FindColumn(v.Name); err != nil {
			g.err = errors.Trace(err)
		} else if col == nil {
			for _, field := range g.fields {
				if field.WildCard == nil && v.Name.Table.L == "" && field.AsName.L == v.Name.Name.L {
					return field.Expr, true
				}
			}
			return inNode, false
		}
	}
	return inNode, true
}

func (b *planBuilder) rewriteGbyExprs(p Plan, gby *ast.GroupByClause, fields []*ast.SelectField) (Plan, bool, []expression.Expression) {
	exprs := make([]expression.Expression, 0, len(gby.Items))
	correlated := false
	for _, item := range gby.Items {
		g := &gbyColsReplacer{fields: fields, schema: p.GetSchema()}
		if g.err != nil {
			b.err = errors.Trace(g.err)
			return nil, false, nil
		}
		retExpr, _ := item.Expr.Accept(g)
		expr, np, cor, err := b.rewrite(retExpr.(ast.ExprNode), p, nil)
		if err != nil {
			b.err = errors.Trace(err)
			return nil, false, nil
		}
		exprs = append(exprs, expr)
		correlated = correlated || cor
		p = np
	}
	return p, correlated, exprs
}

func (b *planBuilder) buildNewSelect(sel *ast.SelectStmt) Plan {
	hasAgg := b.detectSelectAgg(sel)
	var aggFuncs []*ast.AggregateFuncExpr
	var havingMap, orderMap, totalMap map[*ast.AggregateFuncExpr]int
	var p Plan
	if sel.From != nil {
		var gbyCols []expression.Expression
		var correlated bool
		p = b.buildResultSetNode(sel.From.TableRefs)
		if b.err != nil {
			return nil
		}
		if sel.Where != nil {
			p = b.buildSelection(p, sel.Where, nil)
		}
		if b.err != nil {
			return nil
		}
		if sel.LockTp != ast.SelectLockNone {
			p = b.buildSelectLock(p, sel.LockTp)
			if b.err != nil {
				return nil
			}
		}
		if hasAgg {
			if sel.GroupBy != nil {
				p, correlated, gbyCols = b.rewriteGbyExprs(p, sel.GroupBy, sel.Fields.Fields)
				if b.err != nil {
					return nil
				}
			}
			aggFuncs, havingMap, orderMap, totalMap = b.extractAggFunc(sel)
		}
		if hasAgg {
			p = b.buildAggregation(p, aggFuncs, gbyCols, correlated)
		}
	} else {
		if hasAgg {
			aggFuncs, havingMap, orderMap, totalMap = b.extractAggFunc(sel)
		}
		if sel.Where != nil {
			p = b.buildTableDual(sel)
		}
		if hasAgg {
			p = b.buildAggregation(p, aggFuncs, nil, false)
		}
	}
	var oldLen int
	p, oldLen = b.buildProjection(p, sel.Fields.Fields, totalMap)
	if b.err != nil {
		return nil
	}
	if sel.Having != nil && !hasAgg {
		replacer := &astColsReplacer{sel: p.(*Projection)}
		sel.Having.Expr.Accept(replacer)
		if replacer.err != nil {
			b.err = errors.Trace(replacer.err)
			return nil
		}
	}
	if sel.OrderBy != nil && !hasAgg {
		replacer := &astColsReplacer{sel: p.(*Projection), orderBy: true}
		for _, item := range sel.OrderBy.Items {
			replacer.inExpr = false
			item.Expr.Accept(replacer)
			if replacer.err != nil {
				b.err = errors.Trace(replacer.err)
				return nil
			}
		}
	}
	if sel.Having != nil {
		p = b.buildSelection(p, sel.Having.Expr, havingMap)
		if b.err != nil {
			return nil
		}
	}
	if sel.Distinct {
		p = b.buildDistinct(p)
		if b.err != nil {
			return nil
		}
	}
	// TODO: implement push order during cbo
	if sel.OrderBy != nil {
		p = b.buildNewSort(p, sel.OrderBy.Items, orderMap)
		if b.err != nil {
			return nil
		}
	}
	if sel.Limit != nil {
		p = b.buildLimit(p, sel.Limit)
		if b.err != nil {
			return nil
		}
	}
	if oldLen != len(p.GetSchema()) {
		return b.buildTrim(p, oldLen)
	}
	return p
}

func (b *planBuilder) buildTrim(p Plan, len int) Plan {
	trunc := &Trim{}
	trunc.id = b.allocID(trunc)
	addChild(trunc, p)
	trunc.SetSchema(p.GetSchema().DeepCopy()[:len])
	trunc.correlated = p.IsCorrelated()
	return trunc
}

func (b *planBuilder) buildNewTableScanPlan(tn *ast.TableName) Plan {
	p := &NewTableScan{
		Table: tn.TableInfo,
	}
	p.id = b.allocID(p)
	// Equal condition contains a column from previous joined table.
	p.RefAccess = false
	rfs := tn.GetResultFields()
	schema := make([]*expression.Column, 0, len(rfs))
	for _, rf := range rfs {
		p.DBName = &rf.DBName
		p.Columns = append(p.Columns, rf.Column)
		schema = append(schema, &expression.Column{
			FromID:  p.id,
			ColName: rf.Column.Name,
			TblName: rf.Table.Name,
			DBName:  rf.DBName,
			RetType: &rf.Column.FieldType})
	}
	p.SetSchema(schema)
	return p
}

func (b *planBuilder) buildApply(p, inner Plan, schema expression.Schema) Plan {
	ap := &Apply{
		InnerPlan:   inner,
		OuterSchema: schema,
	}
	ap.id = b.allocID(ap)
	addChild(ap, p)
	innerSchema := inner.GetSchema().DeepCopy()
	for _, col := range innerSchema {
		col.Auxiliary = true
	}
	ap.SetSchema(append(p.GetSchema().DeepCopy(), innerSchema...))
	ap.correlated = p.IsCorrelated()
	return ap
}

func (b *planBuilder) buildExists(p Plan) Plan {
	exists := &Exists{}
	exists.id = b.allocID(exists)
	addChild(exists, p)
	newCol := &expression.Column{FromID: exists.id, RetType: types.NewFieldType(mysql.TypeTiny), ColName: model.NewCIStr("exists_col")}
	exists.SetSchema([]*expression.Column{newCol})
	exists.correlated = p.IsCorrelated()
	return exists
}

func (b *planBuilder) buildMaxOneRow(p Plan) Plan {
	maxOneRow := &MaxOneRow{}
	maxOneRow.id = b.allocID(maxOneRow)
	addChild(maxOneRow, p)
	maxOneRow.SetSchema(p.GetSchema().DeepCopy())
	maxOneRow.correlated = p.IsCorrelated()
	return maxOneRow
}
