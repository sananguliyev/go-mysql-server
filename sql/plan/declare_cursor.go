// Copyright 2022 Dolthub, Inc.
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
	"io"

	"github.com/dolthub/go-mysql-server/sql/expression"

	"github.com/dolthub/go-mysql-server/sql"
)

// DeclareCursor represents the DECLARE ... CURSOR statement.
type DeclareCursor struct {
	Name   string
	Select sql.Node
	pRef   *expression.ProcedureReference
	id     int
}

var _ sql.Node = (*DeclareCursor)(nil)
var _ sql.DebugStringer = (*DeclareCursor)(nil)
var _ expression.ProcedureReferencable = (*DeclareCursor)(nil)

// NewDeclareCursor returns a new *DeclareCursor node.
func NewDeclareCursor(name string, selectStatement sql.Node) *DeclareCursor {
	return &DeclareCursor{
		Name:   name,
		Select: selectStatement,
	}
}

// Resolved implements the interface sql.Node.
func (d *DeclareCursor) Resolved() bool {
	return d.Select.Resolved()
}

// String implements the interface sql.Node.
func (d *DeclareCursor) String() string {
	return fmt.Sprintf("DECLARE %s CURSOR FOR %s", d.Name, d.Select.String())
}

// DebugString implements the interface sql.DebugStringer.
func (d *DeclareCursor) DebugString() string {
	return fmt.Sprintf("DECLARE %s CURSOR FOR %s", d.Name, sql.DebugString(d.Select))
}

// Schema implements the interface sql.Node.
func (d *DeclareCursor) Schema() sql.Schema {
	return d.Select.Schema()
}

// Children implements the interface sql.Node.
func (d *DeclareCursor) Children() []sql.Node {
	return []sql.Node{d.Select}
}

// WithChildren implements the interface sql.Node.
func (d *DeclareCursor) WithChildren(children ...sql.Node) (sql.Node, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(d, len(children), 1)
	}

	nd := *d
	nd.Select = children[0]
	return &nd, nil
}

// CheckPrivileges implements the interface sql.Node.
func (d *DeclareCursor) CheckPrivileges(ctx *sql.Context, opChecker sql.PrivilegedOperationChecker) bool {
	return d.Select.CheckPrivileges(ctx, opChecker)
}

// RowIter implements the interface sql.Node.
func (d *DeclareCursor) RowIter(ctx *sql.Context, row sql.Row) (sql.RowIter, error) {
	return &declareCursorIter{d}, nil
}

// WithParamReference implements the interface expression.ProcedureReferencable.
func (d *DeclareCursor) WithParamReference(pRef *expression.ProcedureReference) sql.Node {
	nd := *d
	nd.pRef = pRef
	return &nd
}

// WithId returns a new *DeclareCursor containing the given id.
func (d *DeclareCursor) WithId(id int) *DeclareCursor {
	nd := *d
	nd.id = id
	return &nd
}

// declareCursorIter is the sql.RowIter of *DeclareCursor.
type declareCursorIter struct {
	*DeclareCursor
}

var _ sql.RowIter = (*declareCursorIter)(nil)

// Next implements the interface sql.RowIter.
func (d *declareCursorIter) Next(ctx *sql.Context) (sql.Row, error) {
	d.pRef.InitializeCursor(d.id, d.Name, d.Select)
	return nil, io.EOF
}

// Close implements the interface sql.RowIter.
func (d *declareCursorIter) Close(ctx *sql.Context) error {
	return nil
}
