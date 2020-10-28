package analyzer

import (
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/expression"
	"github.com/dolthub/go-mysql-server/sql/plan"
)

// FixFieldIndexesOnExpressions executes FixFieldIndexes on a list of exprs.
func FixFieldIndexesOnExpressions(schema sql.Schema, expressions ...sql.Expression) ([]sql.Expression, error) {
	var result = make([]sql.Expression, len(expressions))
	for i, e := range expressions {
		var err error
		result[i], err = FixFieldIndexes(nil, schema, e)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

// FixFieldIndexes transforms the given expression by correcting the indexes of columns in GetField expressions,
// according to the schema given. Used when combining multiple tables together into a single join result, or when
// otherwise changing / combining schemas in the node tree.
func FixFieldIndexes(scope *Scope, schema sql.Schema, exp sql.Expression) (sql.Expression, error) {
	scopeLen := len(scope.Schema())

	return expression.TransformUp(exp, func(e sql.Expression) (sql.Expression, error) {
		switch e := e.(type) {
		// For each GetField expression, re-index it with the appropriate index from the schema.
		case *expression.GetField:
			for i, col := range schema {
				if e.Name() == col.Name && e.Table() == col.Source {
					return expression.NewGetFieldWithTable(
						scopeLen+i,
						e.Type(),
						e.Table(),
						e.Name(),
						e.IsNullable(),
					), nil
				}
			}

			// If we didn't find the column in the schema of the node itself, look outward in surrounding scopes. Work
			// inner-to-outer, in  accordance with MySQL scope naming precedence rules.
			offset := 0
			for _, n := range scope.InnerToOuter() {
				schema := schemas(n.Children())
				offset += len(schema)
				for i, col := range schema {
					if e.Name() == col.Name && e.Table() == col.Source {
						return expression.NewGetFieldWithTable(
							scopeLen-offset+i,
							e.Type(),
							e.Table(),
							e.Name(),
							e.IsNullable(),
						), nil
					}
				}
			}

			return nil, ErrFieldMissing.New(e.Name())
		}

		return e, nil
	})
}

// schemas returns the schemas for the nodes given appended in to a single one
func schemas(nodes []sql.Node) sql.Schema {
	var schema sql.Schema
	for _, n := range nodes {
		schema = append(schema, n.Schema()...)
	}
	return schema
}

// Transforms the expressions in the Node given, fixing the field indexes.
func FixFieldIndexesForExpressions(node sql.Node, scope *Scope) (sql.Node, error) {
	if _, ok := node.(sql.Expressioner); !ok {
		return node, nil
	}

	var schemas []sql.Schema
	for _, child := range node.Children() {
		schemas = append(schemas, child.Schema())
	}

	if len(schemas) < 1 {
		return node, nil
	}

	n, err := plan.TransformExpressions(node, func(e sql.Expression) (sql.Expression, error) {
		for _, schema := range schemas {
			fixed, err := FixFieldIndexes(scope, schema, e)
			if err == nil {
				return fixed, nil
			}

			if ErrFieldMissing.Is(err) {
				continue
			}

			return nil, err
		}

		return e, nil
	})

	if err != nil {
		return nil, err
	}

	switch j := n.(type) {
	case *plan.InnerJoin:
		cond, err := FixFieldIndexes(scope, j.Schema(), j.Cond)
		if err != nil {
			return nil, err
		}

		n = plan.NewInnerJoin(j.Left, j.Right, cond)
	case *plan.RightJoin:
		cond, err := FixFieldIndexes(scope, j.Schema(), j.Cond)
		if err != nil {
			return nil, err
		}

		n = plan.NewRightJoin(j.Left, j.Right, cond)
	case *plan.LeftJoin:
		cond, err := FixFieldIndexes(scope, j.Schema(), j.Cond)
		if err != nil {
			return nil, err
		}

		n = plan.NewLeftJoin(j.Left, j.Right, cond)
	}

	return n, nil
}
