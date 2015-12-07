package dynamo

import (
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	// "github.com/davecgh/go-spew/spew"
)

// Query represents a request to get one or more items in a table.
// Query uses the DynamoDB query for requests for multiple items, and GetItem for one.
// See: http://docs.aws.amazon.com/amazondynamodb/latest/APIReference/API_Query.html
// and http://docs.aws.amazon.com/amazondynamodb/latest/APIReference/API_GetItem.html
type Query struct {
	table    Table
	startKey map[string]*dynamodb.AttributeValue
	index    string

	hashKey   string
	hashValue *dynamodb.AttributeValue

	rangeKey    string
	rangeValues []*dynamodb.AttributeValue
	rangeOp     Operator

	projection string
	filter     string
	consistent bool
	limit      int64
	order      Order

	subber

	err error
}

var (
	ErrNotFound = errors.New("dynamo: no item found")  // The requested item could not be found.
	ErrTooMany  = errors.New("dynamo: too many items") // One item was requested, but the query returned multiple.
)

// Operator is an operation to apply in key comparisons.
type Operator *string

var (
	// The following operators are used in key comparisons.
	Equal          Operator = Operator(aws.String("EQ"))
	NotEqual                = Operator(aws.String("NE"))
	Less                    = Operator(aws.String("LT"))
	LessOrEqual             = Operator(aws.String("LE"))
	Greater                 = Operator(aws.String("GT"))
	GreaterOrEqual          = Operator(aws.String("GE"))
	BeginsWith              = Operator(aws.String("BEGINS_WITH"))
	Between                 = Operator(aws.String("BETWEEN"))
)

// These can't be used in key comparions, so disable them for now.
// We will probably never need these.
// var (
// 	IsNull      Operator = Operator(aws.String("NULL"))
// 	NotNull              = Operator(aws.String("NOT_NULL"))
// 	Contains             = Operator(aws.String("CONTAINS"))
// 	NotContains          = Operator(aws.String("NOT_CONTAINS"))
// 	In                   = Operator(aws.String("IN"))
// )

// Order is used for specifying the order of results.
type Order *bool

var (
	Ascending  Order = Order(aws.Bool(true))  // ScanIndexForward = true
	Descending Order = Order(aws.Bool(false)) // ScanIndexForward = false
)

var (
	selectAllAttributes      = aws.String("ALL_ATTRIBUTES")
	selectCount              = aws.String("COUNT")
	selectSpecificAttributes = aws.String("SPECIFIC_ATTRIBUTES")
)

// Get creates a new request to get an item.
// Name is the name of the hash key (a.k.a. partition key).
// Value is the value of the hash key.
func (table Table) Get(name string, value interface{}) *Query {
	q := &Query{
		table:   table,
		hashKey: name,
	}
	q.hashValue, q.err = marshal(value, "")
	return q
}

// Range specifies the range key (a.k.a. sort key) or keys to get.
// For single item requests using One, op must be Equal.
// Name is the name of the range key.
// Op specifies the operator to use when comparing values.
func (q *Query) Range(name string, op Operator, values ...interface{}) *Query {
	var err error
	q.rangeKey = name
	q.rangeOp = op
	q.rangeValues, err = marshalSlice(values)
	q.setError(err)
	return q
}

// Index specifies the name of the index that this query will operate on.
func (q *Query) Index(name string) *Query {
	q.index = name
	return q
}

// Project limits the result attributes to the given paths.
func (q *Query) Project(attribs ...string) *Query {
	expr, err := q.subExpr(strings.Join(attribs, ", "), nil)
	q.setError(err)
	q.projection = expr
	return q
}

// Filter takes an expression that all results will be evaluated against.
// Use single quotes to specificy reserved names inline (like 'Count').
// Use the placeholder ? within the expression to substitute values, and use $ for names.
// You need to use quoted or placeholder names when the name is a reserved word in DynamoDB.
func (q *Query) Filter(expr string, args ...interface{}) *Query {
	expr, err := q.subExpr(expr, args...)
	q.setError(err)
	q.filter = expr
	return q
}

// Consistent, if on is true, will make this query a strongly consistent read.
// Queries are eventually consistent by default.
// Strongly consistent queries are slower and more resource-heavy than eventually consistent queries.
func (q *Query) Consistent(on bool) *Query {
	q.consistent = on
	return q
}

// Limit specifies the maximum amount of results to examine.
// If a filter is not specified, the number of results will be limited.
// If a filter is specified, the number of results to consider for filtering will be limited.
func (q *Query) Limit(limit int64) *Query {
	q.limit = limit
	return q
}

// Order specifies the desired result order.
// Requires a range key (a.k.a. partition key) to be specified.
func (q *Query) Order(order Order) *Query {
	q.order = order
	return q
}

// One executes this query and retrieves a single result,
// unmarshaling the result to out.
func (q *Query) One(out interface{}) error {
	if q.err != nil {
		return q.err
	}

	// Can we use the GetItem API?
	if q.canGetItem() {
		req := q.getItemInput()

		var res *dynamodb.GetItemOutput
		err := retry(func() error {
			var err error
			res, err = q.table.db.client.GetItem(req)
			if err != nil {
				return err
			}
			if res.Item == nil {
				return ErrNotFound
			}
			return nil
		})
		if err != nil {
			return err
		}

		return unmarshalItem(res.Item, out)
	}

	// If not, try a Query.
	req := q.queryInput()

	var res *dynamodb.QueryOutput
	err := retry(func() error {
		var err error
		res, err = q.table.db.client.Query(req)
		if err != nil {
			return err
		}

		switch {
		case len(res.Items) == 0:
			return ErrNotFound
		case len(res.Items) > 1:
			return ErrTooMany
		case res.LastEvaluatedKey != nil && q.limit != 0:
			return ErrTooMany
		}

		return nil
	})
	if err != nil {
		return err
	}

	return unmarshalItem(res.Items[0], out)
}

// All executes this request and unmarshals all results to out, which must be a pointer to a slice.
func (q *Query) All(out interface{}) error {
	if q.err != nil {
		return q.err
	}

	for {
		req := q.queryInput()

		var res *dynamodb.QueryOutput
		err := retry(func() error {
			var err error
			res, err = q.table.db.client.Query(req)
			if err != nil {
				return err
			}

			for _, item := range res.Items {
				if err := unmarshalAppend(item, out); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}

		// do we need to check for more results?
		q.startKey = res.LastEvaluatedKey
		if res.LastEvaluatedKey == nil || q.limit > 0 {
			break
		}
	}

	return nil
}

// Count executes this request, returning the number of results.
func (q *Query) Count() (int64, error) {
	if q.err != nil {
		return 0, q.err
	}

	var count int64
	var res *dynamodb.QueryOutput
	for {
		req := q.queryInput()
		req.Select = selectCount

		err := retry(func() error {
			var err error
			res, err = q.table.db.client.Query(req)
			if err != nil {
				return err
			}
			if res.Count == nil {
				return errors.New("nil count")
			}
			count += *res.Count
			return nil
		})
		if err != nil {
			return 0, err
		}

		q.startKey = res.LastEvaluatedKey
		if res.LastEvaluatedKey == nil || q.limit > 0 {
			break
		}
	}

	return count, nil
}

// can we use the get item API?
func (q *Query) canGetItem() bool {
	switch {
	case q.rangeOp != nil && q.rangeOp != Equal:
		return false
	case q.index != "":
		return false
	case q.filter != "":
		return false
	}
	return true
}

func (q *Query) queryInput() *dynamodb.QueryInput {
	req := &dynamodb.QueryInput{
		TableName:                 &q.table.name,
		KeyConditions:             q.keyConditions(),
		ExclusiveStartKey:         q.startKey,
		ExpressionAttributeNames:  q.nameExpr,
		ExpressionAttributeValues: q.valueExpr,
	}
	if q.consistent {
		req.ConsistentRead = &q.consistent
	}
	if q.limit > 0 {
		req.Limit = &q.limit
	}
	if q.projection != "" {
		req.ProjectionExpression = &q.projection
	}
	if q.filter != "" {
		req.FilterExpression = &q.filter
	}
	if q.index != "" {
		req.IndexName = &q.index
	}
	if q.order != nil {
		req.ScanIndexForward = q.order
	}
	return req
}

func (q *Query) keyConditions() map[string]*dynamodb.Condition {
	conds := map[string]*dynamodb.Condition{
		q.hashKey: &dynamodb.Condition{
			AttributeValueList: []*dynamodb.AttributeValue{q.hashValue},
			ComparisonOperator: Equal,
		},
	}
	if q.rangeKey != "" && q.rangeOp != nil {
		conds[q.rangeKey] = &dynamodb.Condition{
			AttributeValueList: q.rangeValues,
			ComparisonOperator: q.rangeOp,
		}
	}
	return conds
}

func (q *Query) getItemInput() *dynamodb.GetItemInput {
	req := &dynamodb.GetItemInput{
		TableName: &q.table.name,
		Key:       q.keys(),
		ExpressionAttributeNames: q.nameExpr,
	}
	if q.consistent {
		req.ConsistentRead = &q.consistent
	}
	if q.projection != "" {
		req.ProjectionExpression = &q.projection
	}
	return req
}

func (q *Query) keys() map[string]*dynamodb.AttributeValue {
	keys := map[string]*dynamodb.AttributeValue{
		q.hashKey: q.hashValue,
	}
	if q.rangeKey != "" && len(q.rangeValues) > 0 {
		keys[q.rangeKey] = q.rangeValues[0]
	}
	return keys
}

func (q *Query) keysAndAttribs() *dynamodb.KeysAndAttributes {
	kas := &dynamodb.KeysAndAttributes{
		Keys: []map[string]*dynamodb.AttributeValue{q.keys()},
		ExpressionAttributeNames: q.nameExpr,
		ConsistentRead:           &q.consistent,
	}
	if q.projection != "" {
		kas.ProjectionExpression = &q.projection
	}
	return kas
}

func (q *Query) setError(err error) {
	if err != nil {
		q.err = err
	}
}
