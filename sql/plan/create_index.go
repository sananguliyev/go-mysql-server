// Copyright 2020-2021 Dolthub, Inc.
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

package plan

import (
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	errors "gopkg.in/src-d/go-errors.v1"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/expression"
	"github.com/dolthub/go-mysql-server/sql/transform"
)

var (
	// ErrNotIndexable is returned when the table is not indexable.
	ErrNotIndexable = errors.NewKind("the table is not indexable")

	// ErrInvalidIndexDriver is returned when the index driver can't be found.
	ErrInvalidIndexDriver = errors.NewKind("invalid driver index %q")

	// ErrExprTypeNotIndexable is returned when the expression type cannot be
	// indexed, such as BLOB or JSON.
	ErrExprTypeNotIndexable = errors.NewKind("expression %q with type %s cannot be indexed")
)

// CreateIndex is a node to create an index.
type CreateIndex struct {
	Name            string
	Table           sql.Node
	Exprs           []sql.Expression
	Driver          string
	Config          map[string]string
	Catalog         sql.Catalog
	CurrentDatabase string
}

var _ sql.Databaseable = (*CreateIndex)(nil)

// NewCreateIndex creates a new CreateIndex node.
func NewCreateIndex(
	name string,
	table sql.Node,
	exprs []sql.Expression,
	driver string,
	config map[string]string,
) *CreateIndex {
	return &CreateIndex{
		Name:   name,
		Table:  table,
		Exprs:  exprs,
		Driver: driver,
		Config: config,
	}
}

func (c *CreateIndex) Database() string { return c.CurrentDatabase }

// Children implements the Node interface.
func (c *CreateIndex) Children() []sql.Node { return []sql.Node{c.Table} }

// Resolved implements the Node interface.
func (c *CreateIndex) Resolved() bool {
	if !c.Table.Resolved() {
		return false
	}

	for _, e := range c.Exprs {
		if !e.Resolved() {
			return false
		}
	}

	return true
}

func getIndexableTable(t sql.Table) (sql.DriverIndexableTable, error) {
	switch t := t.(type) {
	case sql.DriverIndexableTable:
		return t, nil
	case sql.TableWrapper:
		return getIndexableTable(t.Underlying())
	default:
		return nil, ErrNotIndexable.New()
	}
}

func getChecksumable(t sql.Table) sql.Checksumable {
	switch t := t.(type) {
	case sql.Checksumable:
		return t
	case sql.TableWrapper:
		return getChecksumable(t.Underlying())
	default:
		return nil
	}
}

// RowIter implements the Node interface.
func (c *CreateIndex) RowIter(ctx *sql.Context, row sql.Row) (sql.RowIter, error) {
	table, ok := c.Table.(*ResolvedTable)
	if !ok {
		return nil, ErrNotIndexable.New()
	}

	indexable, err := getIndexableTable(table.Table)
	if err != nil {
		return nil, err
	}

	var driver sql.IndexDriver
	if c.Driver == "" {
		driver = ctx.GetIndexRegistry().DefaultIndexDriver()
	} else {
		driver = ctx.GetIndexRegistry().IndexDriver(c.Driver)
	}

	if driver == nil {
		return nil, ErrInvalidIndexDriver.New(c.Driver)
	}

	columns, exprs, err := GetColumnsAndPrepareExpressions(ctx, c.Exprs)
	if err != nil {
		return nil, err
	}

	for _, e := range exprs {
		if sql.IsBlobType(e.Type()) || sql.IsJSON(e.Type()) {
			return nil, ErrExprTypeNotIndexable.New(e, e.Type())
		}
	}

	if ch := getChecksumable(table.Table); ch != nil {
		c.Config[sql.ChecksumKey], err = ch.Checksum()
		if err != nil {
			return nil, err
		}
	}

	index, err := driver.Create(
		c.CurrentDatabase,
		table.Name(),
		c.Name,
		exprs,
		c.Config,
	)
	if err != nil {
		return nil, err
	}

	iter, err := indexable.IndexKeyValues(ctx, columns)
	if err != nil {
		return nil, err
	}

	iter = &EvalPartitionKeyValueIter{
		columns: columns,
		exprs:   exprs,
		iter:    iter,
	}

	created, ready, err := ctx.GetIndexRegistry().AddIndex(index)
	if err != nil {
		return nil, err
	}

	log := logrus.WithFields(logrus.Fields{
		"id":     index.ID(),
		"driver": index.Driver(),
	})

	createIndex := func() {
		c.createIndex(ctx, log, driver, index, iter, created, ready)
	}

	log.Info("starting to save the index")

	createIndex()

	return sql.RowsToRowIter(), nil
}

func (c *CreateIndex) createIndex(
	ctx *sql.Context,
	log *logrus.Entry,
	driver sql.IndexDriver,
	index sql.DriverIndex,
	iter sql.PartitionIndexKeyValueIter,
	done chan<- struct{},
	ready <-chan struct{},
) {
	span, ctx := ctx.Span("plan.createIndex",
		trace.WithAttributes(
			attribute.String("index", index.ID()),
			attribute.String("table", index.Table()),
			attribute.String("driver", index.Driver()),
		),
	)
	defer span.End()

	l := log.WithField("id", index.ID())

	err := driver.Save(ctx, index, newLoggingPartitionKeyValueIter(l, iter))
	close(done)

	if err != nil {
		span.RecordError(err)

		ctx.Error(0, "unable to save the index: %s", err)
		logrus.WithField("err", err).Error("unable to save the index")

		deleted, err := ctx.GetIndexRegistry().DeleteIndex(index.Database(), index.ID(), true)
		if err != nil {
			ctx.Error(0, "unable to delete index: %s", err)
			logrus.WithField("err", err).Error("unable to delete the index")
		} else {
			<-deleted
		}
	} else {
		<-ready
		log.Info("index successfully created")
	}
}

// Schema implements the Node interface.
func (c *CreateIndex) Schema() sql.Schema { return nil }

func (c *CreateIndex) String() string {
	var exprs = make([]string, len(c.Exprs))
	for i, e := range c.Exprs {
		exprs[i] = e.String()
	}

	pr := sql.NewTreePrinter()
	_ = pr.WriteNode("CreateIndex(%s)", c.Name)
	_ = pr.WriteChildren(
		fmt.Sprintf("USING %s", c.Driver),
		fmt.Sprintf("Expressions (%s)", strings.Join(exprs, ", ")),
		c.Table.String(),
	)
	return pr.String()
}

// Expressions implements the Expressioner interface.
func (c *CreateIndex) Expressions() []sql.Expression {
	return c.Exprs
}

// WithExpressions implements the Expressioner interface.
func (c *CreateIndex) WithExpressions(exprs ...sql.Expression) (sql.Node, error) {
	if len(exprs) != len(c.Exprs) {
		return nil, sql.ErrInvalidChildrenNumber.New(c, len(exprs), len(c.Exprs))
	}

	nc := *c
	nc.Exprs = exprs
	return &nc, nil
}

// WithChildren implements the Node interface.
func (c *CreateIndex) WithChildren(children ...sql.Node) (sql.Node, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(c, len(children), 1)
	}

	nc := *c
	nc.Table = children[0]
	return &nc, nil
}

// CheckPrivileges implements the interface sql.Node.
func (c *CreateIndex) CheckPrivileges(ctx *sql.Context, opChecker sql.PrivilegedOperationChecker) bool {
	return opChecker.UserHasPrivileges(ctx,
		sql.NewPrivilegedOperation(getDatabaseName(c.Table), getTableName(c.Table), "", sql.PrivilegeType_Index))
}

// GetColumnsAndPrepareExpressions extracts the unique columns required by all
// those expressions and fixes the indexes of the GetFields in the expressions
// to match a row with only the returned columns in that same order.
func GetColumnsAndPrepareExpressions(
	ctx *sql.Context,
	exprs []sql.Expression,
) ([]string, []sql.Expression, error) {
	var columns []string
	var seen = make(map[string]int)
	var expressions = make([]sql.Expression, len(exprs))

	for i, e := range exprs {
		ex, _, err := transform.Expr(e, func(e sql.Expression) (sql.Expression, transform.TreeIdentity, error) {
			gf, ok := e.(*expression.GetField)
			if !ok {
				return e, transform.SameTree, nil
			}

			var idx int
			if j, ok := seen[gf.Name()]; ok {
				idx = j
			} else {
				idx = len(columns)
				columns = append(columns, gf.Name())
				seen[gf.Name()] = idx
			}

			return expression.NewGetFieldWithTable(
				idx,
				gf.Type(),
				gf.Table(),
				gf.Name(),
				gf.IsNullable(),
			), transform.NewTree, nil
		})

		if err != nil {
			return nil, nil, err
		}

		expressions[i] = ex
	}

	return columns, expressions, nil
}

type EvalPartitionKeyValueIter struct {
	iter    sql.PartitionIndexKeyValueIter
	columns []string
	exprs   []sql.Expression
}

func NewEvalPartitionKeyValueIter(iter sql.PartitionIndexKeyValueIter, columns []string, exprs []sql.Expression) *EvalPartitionKeyValueIter {
	return &EvalPartitionKeyValueIter{
		iter:    iter,
		columns: columns,
		exprs:   exprs,
	}
}

func (i *EvalPartitionKeyValueIter) Next(ctx *sql.Context) (sql.Partition, sql.IndexKeyValueIter, error) {
	p, iter, err := i.iter.Next(ctx)
	if err != nil {
		return nil, nil, err
	}

	return p, &evalKeyValueIter{
		columns: i.columns,
		exprs:   i.exprs,
		iter:    iter,
	}, nil
}

func (i *EvalPartitionKeyValueIter) Close(ctx *sql.Context) error {
	return i.iter.Close(ctx)
}

type evalKeyValueIter struct {
	iter    sql.IndexKeyValueIter
	columns []string
	exprs   []sql.Expression
}

func (i *evalKeyValueIter) Next(ctx *sql.Context) ([]interface{}, []byte, error) {
	vals, loc, err := i.iter.Next(ctx)
	if err != nil {
		return nil, nil, err
	}

	row := sql.NewRow(vals...)
	evals := make([]interface{}, len(i.exprs))
	for j, ex := range i.exprs {
		eval, err := ex.Eval(ctx, row)
		if err != nil {
			return nil, nil, err
		}

		evals[j] = eval
	}

	return evals, loc, nil
}

func (i *evalKeyValueIter) Close(ctx *sql.Context) error {
	return i.iter.Close(ctx)
}

type loggingPartitionKeyValueIter struct {
	log  *logrus.Entry
	iter sql.PartitionIndexKeyValueIter
	rows uint64
}

func newLoggingPartitionKeyValueIter(
	log *logrus.Entry,
	iter sql.PartitionIndexKeyValueIter,
) *loggingPartitionKeyValueIter {
	return &loggingPartitionKeyValueIter{
		log:  log,
		iter: iter,
	}
}

func (i *loggingPartitionKeyValueIter) Next(ctx *sql.Context) (sql.Partition, sql.IndexKeyValueIter, error) {
	p, iter, err := i.iter.Next(ctx)
	if err != nil {
		return nil, nil, err
	}

	return p, newLoggingKeyValueIter(i.log, iter, &i.rows), nil
}

func (i *loggingPartitionKeyValueIter) Close(ctx *sql.Context) error {
	return i.iter.Close(ctx)
}

type loggingKeyValueIter struct {
	span  trace.Span
	log   *logrus.Entry
	iter  sql.IndexKeyValueIter
	rows  *uint64
	start time.Time
}

func newLoggingKeyValueIter(
	log *logrus.Entry,
	iter sql.IndexKeyValueIter,
	rows *uint64,
) *loggingKeyValueIter {
	return &loggingKeyValueIter{
		log:   log,
		iter:  iter,
		start: time.Now(),
		rows:  rows,
	}
}

func (i *loggingKeyValueIter) Next(ctx *sql.Context) ([]interface{}, []byte, error) {
	if i.span == nil {
		i.span, ctx = ctx.Span("plan.createIndex.iterator", trace.WithAttributes(attribute.Int64("start", int64(*i.rows))))
	}

	(*i.rows)++
	if *i.rows%sql.IndexBatchSize == 0 {
		duration := time.Since(i.start)

		i.log.WithFields(logrus.Fields{
			"duration": duration,
			"rows":     *i.rows,
		}).Debugf("still creating index")

		if i.span != nil {
			i.span.SetAttributes(attribute.Stringer("duration", duration))
			i.span.End()
			i.span = nil
		}

		i.start = time.Now()
	}

	val, loc, err := i.iter.Next(ctx)
	if err != nil {
		i.span.RecordError(err)
		i.span.End()
		i.span = nil
	}

	return val, loc, err
}

func (i *loggingKeyValueIter) Close(ctx *sql.Context) error {
	return i.iter.Close(ctx)
}
