package database

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/buger/jsonparser"
	"github.com/jensneuse/abstractlogger"
	"github.com/tidwall/sjson"

	"github.com/wundergraph/graphql-go-tools/pkg/ast"
	"github.com/wundergraph/graphql-go-tools/pkg/astnormalization"
	"github.com/wundergraph/graphql-go-tools/pkg/astparser"
	"github.com/wundergraph/graphql-go-tools/pkg/astprinter"
	"github.com/wundergraph/graphql-go-tools/pkg/asttransform"
	"github.com/wundergraph/graphql-go-tools/pkg/engine/datasource/httpclient"
	"github.com/wundergraph/graphql-go-tools/pkg/engine/plan"
	"github.com/wundergraph/graphql-go-tools/pkg/engine/resolve"
	"github.com/wundergraph/graphql-go-tools/pkg/operationreport"

	"github.com/wundergraph/wundergraph/pkg/pool"
)

type Planner struct {
	visitor                    *plan.Visitor
	config                     Configuration
	id                         string
	upstreamOperation          *ast.Document
	upstreamVariables          []byte
	nodes                      []ast.Node
	variables                  resolve.Variables
	lastFieldEnclosingTypeName string
	disallowSingleFlight       bool
	client                     *http.Client
	isNested                   bool   // isNested - flags that datasource is nested e.g. field with datasource is not on a query type
	rootTypeName               string // rootTypeName - holds name of top level type
	rootFieldName              string // rootFieldName - holds name of root type field
	rootFieldRef               int    // rootFieldRef - holds ref of root type field

	inlinedVariables []inlinedVariable

	engineFactory *LazyEngineFactory

	debug bool
	log   abstractlogger.Logger

	insideJsonField bool
	jsonFieldRef    int
}

type inlinedVariable struct {
	name    string
	typeRef int
	isJSON  bool
}

func (p *Planner) DownstreamResponseFieldAlias(downstreamFieldRef int) (alias string, exists bool) {

	// If there's no alias but the downstream Query re-uses the same path on different root fields,
	// we rewrite the downstream Query using an alias so that we can have an aliased Query to the upstream
	// while keeping a non aliased Query to the downstream but with a path rewrite on an existing root field.

	fieldName := p.visitor.Operation.FieldNameUnsafeString(downstreamFieldRef)

	if p.visitor.Operation.FieldAliasIsDefined(downstreamFieldRef) {
		return "", false
	}

	typeName := p.visitor.Walker.EnclosingTypeDefinition.NameString(p.visitor.Definition)
	for i := range p.visitor.Config.Fields {
		if p.visitor.Config.Fields[i].TypeName == typeName &&
			p.visitor.Config.Fields[i].FieldName == fieldName &&
			len(p.visitor.Config.Fields[i].Path) == 1 {

			if p.visitor.Config.Fields[i].Path[0] != fieldName {
				aliasBytes := p.visitor.Operation.FieldNameBytes(downstreamFieldRef)
				return string(aliasBytes), true
			}
			break
		}
	}
	return "", false
}

func (p *Planner) DataSourcePlanningBehavior() plan.DataSourcePlanningBehavior {
	return plan.DataSourcePlanningBehavior{
		MergeAliasedRootNodes:      true,
		OverrideFieldPathFromAlias: false,
	}
}

type Configuration struct {
	DatabaseURL         string
	PrismaSchema        string
	GraphqlSchema       string
	CloseTimeoutSeconds int64
	JsonTypeFields      []SingleTypeField
	JsonInputVariables  []string
	WunderGraphDir      string
}

type SingleTypeField struct {
	TypeName  string
	FieldName string
}

func ConfigJson(config Configuration) json.RawMessage {
	out, _ := json.Marshal(config)
	return out
}

func (p *Planner) Register(visitor *plan.Visitor, configuration plan.DataSourceConfiguration, isNested bool) error {
	p.visitor = visitor
	p.visitor.Walker.RegisterDocumentVisitor(p)
	p.visitor.Walker.RegisterFieldVisitor(p)
	p.visitor.Walker.RegisterOperationDefinitionVisitor(p)
	p.visitor.Walker.RegisterSelectionSetVisitor(p)
	p.visitor.Walker.RegisterEnterArgumentVisitor(p)
	p.visitor.Walker.RegisterInlineFragmentVisitor(p)

	err := json.Unmarshal(configuration.Custom, &p.config)
	if err != nil {
		return err
	}

	if p.config.CloseTimeoutSeconds == 0 {
		p.config.CloseTimeoutSeconds = 10
	}

	p.isNested = isNested

	return nil
}

type fetchInput struct {
	Query     string          `json:"query"`
	Variables json.RawMessage `json:"variables"`
}

func (p *Planner) ConfigureFetch() plan.FetchConfiguration {

	operation := string(p.printOperation())

	input := fetchInput{
		Query:     operation,
		Variables: p.upstreamVariables,
	}

	for _, inlinedVariable := range p.inlinedVariables {
		currentName := "$" + inlinedVariable.name
		var (
			renderer resolve.VariableRenderer
			err      error
		)
		if inlinedVariable.isJSON {
			renderer = resolve.NewGraphQLVariableRenderer(`{"type":"string"}`)
		} else {
			renderer, err = resolve.NewGraphQLVariableRendererFromTypeRefWithoutValidation(p.visitor.Operation, p.visitor.Definition, inlinedVariable.typeRef)
			if err != nil {
				continue
			}
		}
		variable := &resolve.ContextVariable{
			Path:     []string{inlinedVariable.name},
			Renderer: renderer,
		}
		replacement, _ := p.variables.AddVariable(variable)
		input.Query = strings.Replace(input.Query, currentName, replacement, -1)
	}

	rawInput, err := json.Marshal(input)
	if err != nil {
		rawInput = []byte(`{"error":` + err.Error() + `}`)
	}

	return plan.FetchConfiguration{
		Input: string(rawInput),
		DataSource: &Source{
			engine: p.engineFactory.Engine(p.config.PrismaSchema, p.config.WunderGraphDir, p.config.CloseTimeoutSeconds),
			debug:  p.debug,
			log:    p.log,
		},
		Variables:            p.variables,
		DisallowSingleFlight: p.disallowSingleFlight,
		ProcessResponseConfig: resolve.ProcessResponseConfig{
			ExtractGraphqlResponse: true,
		},
	}
}

func (p *Planner) ConfigureSubscription() plan.SubscriptionConfiguration {

	input := httpclient.SetInputBodyWithPath(nil, p.upstreamVariables, "variables")
	input = httpclient.SetInputBodyWithPath(input, p.printOperation(), "query")

	return plan.SubscriptionConfiguration{
		Input:      string(input),
		DataSource: nil,
		Variables:  p.variables,
	}
}

func (p *Planner) EnterOperationDefinition(ref int) {
	operationType := p.visitor.Operation.OperationDefinitions[ref].OperationType
	if p.isNested {
		operationType = ast.OperationTypeQuery
	}
	definition := p.upstreamOperation.AddOperationDefinitionToRootNodes(ast.OperationDefinition{
		OperationType: operationType,
	})
	p.disallowSingleFlight = operationType == ast.OperationTypeMutation
	p.nodes = append(p.nodes, definition)
}

func (p *Planner) LeaveOperationDefinition(_ int) {
	p.nodes = p.nodes[:len(p.nodes)-1]
}

func (p *Planner) EnterSelectionSet(ref int) {

	if p.insideJsonField {
		return
	}

	parent := p.nodes[len(p.nodes)-1]
	set := p.upstreamOperation.AddSelectionSet()
	switch parent.Kind {
	case ast.NodeKindOperationDefinition:
		p.upstreamOperation.OperationDefinitions[parent.Ref].HasSelections = true
		p.upstreamOperation.OperationDefinitions[parent.Ref].SelectionSet = set.Ref
	case ast.NodeKindField:
		p.upstreamOperation.Fields[parent.Ref].HasSelections = true
		p.upstreamOperation.Fields[parent.Ref].SelectionSet = set.Ref
	case ast.NodeKindInlineFragment:
		p.upstreamOperation.InlineFragments[parent.Ref].HasSelections = true
		p.upstreamOperation.InlineFragments[parent.Ref].SelectionSet = set.Ref
	}
	p.nodes = append(p.nodes, set)
	for _, selectionRef := range p.visitor.Operation.SelectionSets[ref].SelectionRefs {
		if p.visitor.Operation.Selections[selectionRef].Kind == ast.SelectionKindField {
			if p.visitor.Operation.FieldNameUnsafeString(p.visitor.Operation.Selections[selectionRef].Ref) == "__typename" {
				field := p.upstreamOperation.AddField(ast.Field{
					Name: p.upstreamOperation.Input.AppendInputString("__typename"),
				})
				p.upstreamOperation.AddSelection(set.Ref, ast.Selection{
					Ref:  field.Ref,
					Kind: ast.SelectionKindField,
				})
			}
		}
	}
}

func (p *Planner) LeaveSelectionSet(ref int) {

	if p.insideJsonField {
		return
	}

	p.nodes = p.nodes[:len(p.nodes)-1]
}

func (p *Planner) EnterInlineFragment(ref int) {

	if p.insideJsonField {
		return
	}

	typeCondition := p.visitor.Operation.InlineFragmentTypeConditionName(ref)
	if typeCondition == nil {
		return
	}

	inlineFragment := p.upstreamOperation.AddInlineFragment(ast.InlineFragment{
		TypeCondition: ast.TypeCondition{
			Type: p.upstreamOperation.AddNamedType(typeCondition),
		},
	})

	selection := ast.Selection{
		Kind: ast.SelectionKindInlineFragment,
		Ref:  inlineFragment,
	}

	// add __typename field to selection set which contains typeCondition
	// so that the resolver can distinguish between the response types
	typeNameField := p.upstreamOperation.AddField(ast.Field{
		Name: p.upstreamOperation.Input.AppendInputBytes([]byte("__typename")),
	})
	p.upstreamOperation.AddSelection(p.nodes[len(p.nodes)-1].Ref, ast.Selection{
		Kind: ast.SelectionKindField,
		Ref:  typeNameField.Ref,
	})

	p.upstreamOperation.AddSelection(p.nodes[len(p.nodes)-1].Ref, selection)
	p.nodes = append(p.nodes, ast.Node{Kind: ast.NodeKindInlineFragment, Ref: inlineFragment})
}

func (p *Planner) LeaveInlineFragment(ref int) {

	if p.insideJsonField {
		return
	}

	if p.nodes[len(p.nodes)-1].Kind != ast.NodeKindInlineFragment {
		return
	}
	p.nodes = p.nodes[:len(p.nodes)-1]
}

func (p *Planner) EnterField(ref int) {

	if p.insideJsonField {
		return
	}

	fieldName := p.visitor.Operation.FieldNameString(ref)
	enclosingTypeName := p.visitor.Walker.EnclosingTypeDefinition.NameString(p.visitor.Definition)
	for i := range p.config.JsonTypeFields {
		if p.config.JsonTypeFields[i].TypeName == enclosingTypeName && p.config.JsonTypeFields[i].FieldName == fieldName {
			p.insideJsonField = true
			p.jsonFieldRef = ref
			p.addJsonField(ref)
			return
		}
	}

	// store root field name and ref
	if p.rootFieldName == "" {
		p.rootFieldName = fieldName
		p.rootFieldRef = ref
	}
	// store root type name
	if p.rootTypeName == "" {
		p.rootTypeName = p.visitor.Walker.EnclosingTypeDefinition.NameString(p.visitor.Definition)
	}

	p.lastFieldEnclosingTypeName = p.visitor.Walker.EnclosingTypeDefinition.NameString(p.visitor.Definition)

	p.addField(ref)

	upstreamFieldRef := p.nodes[len(p.nodes)-1].Ref
	typeName := p.lastFieldEnclosingTypeName

	fieldConfiguration := p.visitor.Config.Fields.ForTypeField(typeName, fieldName)
	if fieldConfiguration == nil {
		return
	}
	for i := range fieldConfiguration.Arguments {
		argumentConfiguration := fieldConfiguration.Arguments[i]
		p.configureArgument(upstreamFieldRef, ref, *fieldConfiguration, argumentConfiguration)
	}
}

func (p *Planner) addJsonField(ref int) {
	fieldName := p.visitor.Operation.FieldNameString(ref)
	field := p.upstreamOperation.AddField(ast.Field{
		Name: p.upstreamOperation.Input.AppendInputString(fieldName),
	})
	selection := ast.Selection{
		Kind: ast.SelectionKindField,
		Ref:  field.Ref,
	}
	p.upstreamOperation.AddSelection(p.nodes[len(p.nodes)-1].Ref, selection)
}

func (p *Planner) LeaveField(ref int) {
	if p.insideJsonField {
		if p.jsonFieldRef == ref {
			p.insideJsonField = false
			p.jsonFieldRef = 0
		}
		return
	}
	p.nodes = p.nodes[:len(p.nodes)-1]
}

func (p *Planner) EnterArgument(ref int) {
	if p.insideJsonField {
		return
	}
}

func (p *Planner) EnterDocument(operation, definition *ast.Document) {
	if p.upstreamOperation == nil {
		p.upstreamOperation = ast.NewDocument()
	} else {
		p.upstreamOperation.Reset()
	}
	p.nodes = p.nodes[:0]
	p.upstreamVariables = nil
	p.variables = p.variables[:0]
	p.disallowSingleFlight = false

	// reset information about root type
	p.rootTypeName = ""
	p.rootFieldName = ""
	p.rootFieldRef = -1
}

func (p *Planner) LeaveDocument(operation, definition *ast.Document) {

}

func (p *Planner) addOneTypeInlineFragment() {
	selectionSet := p.upstreamOperation.AddSelectionSet()
	typeRef := p.upstreamOperation.AddNamedType([]byte(p.lastFieldEnclosingTypeName))
	inlineFragment := p.upstreamOperation.AddInlineFragment(ast.InlineFragment{
		HasSelections: true,
		SelectionSet:  selectionSet.Ref,
		TypeCondition: ast.TypeCondition{
			Type: typeRef,
		},
	})
	p.upstreamOperation.AddSelection(p.nodes[len(p.nodes)-1].Ref, ast.Selection{
		Kind: ast.SelectionKindInlineFragment,
		Ref:  inlineFragment,
	})
	p.nodes = append(p.nodes, selectionSet)
}

func (p *Planner) addEntitiesSelectionSet() {

	// $representations
	representationsLiteral := p.upstreamOperation.Input.AppendInputString("representations")
	representationsVariable := p.upstreamOperation.AddVariableValue(ast.VariableValue{
		Name: representationsLiteral,
	})
	representationsArgument := p.upstreamOperation.AddArgument(ast.Argument{
		Name: representationsLiteral,
		Value: ast.Value{
			Kind: ast.ValueKindVariable,
			Ref:  representationsVariable,
		},
	})

	// _entities
	entitiesSelectionSet := p.upstreamOperation.AddSelectionSet()
	entitiesField := p.upstreamOperation.AddField(ast.Field{
		Name:          p.upstreamOperation.Input.AppendInputString("_entities"),
		HasSelections: true,
		HasArguments:  true,
		Arguments: ast.ArgumentList{
			Refs: []int{representationsArgument},
		},
		SelectionSet: entitiesSelectionSet.Ref,
	})
	p.upstreamOperation.AddSelection(p.nodes[len(p.nodes)-1].Ref, ast.Selection{
		Kind: ast.SelectionKindField,
		Ref:  entitiesField.Ref,
	})
	p.nodes = append(p.nodes, entitiesField, entitiesSelectionSet)
}

func (p *Planner) addRepresentationsVariableDefinition() {
	anyType := p.upstreamOperation.AddNamedType([]byte("_Any"))
	nonNullAnyType := p.upstreamOperation.AddType(ast.Type{
		TypeKind: ast.TypeKindNonNull,
		OfType:   anyType,
	})
	listOfNonNullAnyType := p.upstreamOperation.AddType(ast.Type{
		TypeKind: ast.TypeKindList,
		OfType:   nonNullAnyType,
	})
	nonNullListOfNonNullAnyType := p.upstreamOperation.AddType(ast.Type{
		TypeKind: ast.TypeKindNonNull,
		OfType:   listOfNonNullAnyType,
	})
	representationsVariable := p.upstreamOperation.AddVariableValue(ast.VariableValue{
		Name: p.upstreamOperation.Input.AppendInputBytes([]byte("representations")),
	})
	p.upstreamOperation.AddVariableDefinitionToOperationDefinition(p.nodes[0].Ref, representationsVariable, nonNullListOfNonNullAnyType)
}

func (p *Planner) isNestedRequest() bool {
	for i := range p.nodes {
		if p.nodes[i].Kind == ast.NodeKindField {
			return false
		}
	}
	selectionSetAncestors := 0
	for i := range p.visitor.Walker.Ancestors {
		if p.visitor.Walker.Ancestors[i].Kind == ast.NodeKindSelectionSet {
			selectionSetAncestors++
			if selectionSetAncestors == 2 {
				return true
			}
		}
	}
	return false
}

func (p *Planner) configureArgument(upstreamFieldRef, downstreamFieldRef int, fieldConfig plan.FieldConfiguration, argumentConfiguration plan.ArgumentConfiguration) {
	switch argumentConfiguration.SourceType {
	case plan.FieldArgumentSource:
		p.configureFieldArgumentSource(upstreamFieldRef, downstreamFieldRef, argumentConfiguration.Name, argumentConfiguration.SourcePath)
	case plan.ObjectFieldSource:
		p.configureObjectFieldSource(upstreamFieldRef, downstreamFieldRef, fieldConfig, argumentConfiguration)
	}
}

func (p *Planner) configureFieldArgumentSource(upstreamFieldRef, downstreamFieldRef int, argumentName string, sourcePath []string) {
	fieldArgument, ok := p.visitor.Operation.FieldArgument(downstreamFieldRef, []byte(argumentName))
	if !ok {
		return
	}
	value := p.visitor.Operation.ArgumentValue(fieldArgument)
	if value.Kind != ast.ValueKindVariable {
		p.applyInlineFieldArgument(upstreamFieldRef, downstreamFieldRef, argumentName, sourcePath)
		return
	}
	variableName := p.visitor.Operation.VariableValueNameBytes(value.Ref)
	variableNameStr := p.visitor.Operation.VariableValueNameString(value.Ref)

	variableDefinition, ok := p.visitor.Operation.VariableDefinitionByNameAndOperation(p.visitor.Walker.Ancestors[0].Ref, variableName)
	if !ok {
		return
	}

	variableDefinitionType := p.visitor.Operation.VariableDefinitions[variableDefinition].Type

	_, argRef := p.upstreamOperation.AddVariableValueArgument([]byte(argumentName), variableName) // add the argument to the field, but don't redefine it
	p.upstreamOperation.AddArgumentToField(upstreamFieldRef, argRef)

	typeName := p.visitor.Operation.ResolveTypeNameString(variableDefinitionType)
	isJSON := false
	for i := range p.config.JsonInputVariables {
		if typeName == p.config.JsonInputVariables[i] {
			isJSON = true
		}
	}

	p.inlinedVariables = append(p.inlinedVariables, inlinedVariable{
		name:    variableNameStr,
		typeRef: variableDefinitionType,
		isJSON:  isJSON,
	})
}

func (p *Planner) applyInlineFieldArgument(upstreamField, downstreamField int, argumentName string, sourcePath []string) {
	fieldArgument, ok := p.visitor.Operation.FieldArgument(downstreamField, []byte(argumentName))
	if !ok {
		return
	}
	value := p.visitor.Operation.ArgumentValue(fieldArgument)
	importedValue := p.visitor.Importer.ImportValue(value, p.visitor.Operation, p.upstreamOperation)
	argRef := p.upstreamOperation.AddArgument(ast.Argument{
		Name:  p.upstreamOperation.Input.AppendInputString(argumentName),
		Value: importedValue,
	})
	p.upstreamOperation.AddArgumentToField(upstreamField, argRef)
	p.addVariableDefinitionsRecursively(value, argumentName, sourcePath)
}

func (p *Planner) addVariableDefinitionsRecursively(value ast.Value, argumentName string, sourcePath []string) {
	switch value.Kind {
	case ast.ValueKindObject:
		for _, i := range p.visitor.Operation.ObjectValues[value.Ref].Refs {
			p.addVariableDefinitionsRecursively(p.visitor.Operation.ObjectFields[i].Value, argumentName, sourcePath)
		}
		return
	case ast.ValueKindList:
		for _, i := range p.visitor.Operation.ListValues[value.Ref].Refs {
			p.addVariableDefinitionsRecursively(p.visitor.Operation.Values[i], argumentName, sourcePath)
		}
		return
	case ast.ValueKindVariable:
		// continue after switch
	default:
		return
	}

	variableName := p.visitor.Operation.VariableValueNameBytes(value.Ref)
	variableNameStr := p.visitor.Operation.VariableValueNameString(value.Ref)
	variableDefinition, _ := p.visitor.Operation.VariableDefinitionByNameAndOperation(p.visitor.Walker.Ancestors[0].Ref, variableName)

	variableDefinitionType := p.visitor.Operation.VariableDefinitions[variableDefinition].Type

	typeName := p.visitor.Operation.ResolveTypeNameString(variableDefinitionType)
	isJSON := false
	for i := range p.config.JsonInputVariables {
		if typeName == p.config.JsonInputVariables[i] {
			isJSON = true
		}
	}

	p.inlinedVariables = append(p.inlinedVariables, inlinedVariable{
		name:    variableNameStr,
		typeRef: variableDefinitionType,
		isJSON:  isJSON,
	})
}

func (p *Planner) configureObjectFieldSource(upstreamFieldRef, downstreamFieldRef int, fieldConfiguration plan.FieldConfiguration, argumentConfiguration plan.ArgumentConfiguration) {
	if len(argumentConfiguration.SourcePath) < 1 {
		return
	}

	fieldName := p.visitor.Operation.FieldNameUnsafeString(downstreamFieldRef)

	if len(fieldConfiguration.Path) == 1 {
		fieldName = fieldConfiguration.Path[0]
	}

	queryTypeDefinition, exists := p.visitor.Definition.Index.FirstNodeByNameBytes(p.visitor.Definition.Index.QueryTypeName)
	if !exists {
		return
	}
	argumentDefinition := p.visitor.Definition.NodeFieldDefinitionArgumentDefinitionByName(queryTypeDefinition, []byte(fieldName), []byte(argumentConfiguration.Name))
	if argumentDefinition == -1 {
		return
	}

	argumentType := p.visitor.Definition.InputValueDefinitionType(argumentDefinition)
	variableName := p.upstreamOperation.GenerateUnusedVariableDefinitionName(p.nodes[0].Ref)
	variableValue, argument := p.upstreamOperation.AddVariableValueArgument([]byte(argumentConfiguration.Name), variableName)
	p.upstreamOperation.AddArgumentToField(upstreamFieldRef, argument)
	importedType := p.visitor.Importer.ImportType(argumentType, p.visitor.Definition, p.upstreamOperation)
	p.upstreamOperation.AddVariableDefinitionToOperationDefinition(p.nodes[0].Ref, variableValue, importedType)

	renderer, err := resolve.NewGraphQLVariableRendererFromTypeRef(p.visitor.Operation, p.visitor.Definition, argumentType)
	if err != nil {
		return
	}

	variable := &resolve.ObjectVariable{
		Path:     argumentConfiguration.SourcePath,
		Renderer: renderer,
	}

	objectVariableName, exists := p.variables.AddVariable(variable)
	if !exists {
		p.upstreamVariables, _ = sjson.SetRawBytes(p.upstreamVariables, string(variableName), []byte(objectVariableName))
	}
}

const (
	normalizationFailedErrMsg = "printOperation: normalization failed"
	parseDocumentFailedErrMsg = "printOperation: parse %s failed"
)

// printOperation - prints normalized upstream operation
func (p *Planner) printOperation() []byte {

	buf := &bytes.Buffer{}

	err := astprinter.Print(p.upstreamOperation, nil, buf)
	if err != nil {
		return nil
	}

	rawQuery := buf.Bytes()

	// create empty operation and definition documents
	operation := ast.NewDocument()
	definition := ast.NewDocument()
	report := &operationreport.Report{}
	parser := astparser.NewParser()

	// creates a copy of operation and schema to be able to safely modify them
	definition.Input.ResetInputString(p.config.GraphqlSchema)
	operation.Input.ResetInputBytes(rawQuery)

	parser.Parse(operation, report)
	if report.HasErrors() {
		p.stopWithError(parseDocumentFailedErrMsg, "operation")
		return nil
	}

	parser.Parse(definition, report)
	if report.HasErrors() {
		p.stopWithError(parseDocumentFailedErrMsg, "definition")
		return nil
	}

	err = asttransform.MergeDefinitionWithBaseSchema(definition)
	if err != nil {
		p.stopWithError("merging upstream schema with base schema failed")
		return nil
	}

	// When datasource is nested and definition query type do not contain operation field
	// we have to replace a query type with a current root type
	p.replaceQueryType(definition)

	// normalize upstream operation
	if !p.normalizeOperation(operation, definition, report) {
		p.stopWithError(normalizationFailedErrMsg)
		return nil
	}

	buf.Reset()

	// print upstream operation
	err = astprinter.Print(operation, p.visitor.Definition, buf)
	if err != nil {
		p.stopWithError(normalizationFailedErrMsg)
		return nil
	}

	return buf.Bytes()
}

func (p *Planner) stopWithError(msg string, args ...interface{}) {
	p.visitor.Walker.StopWithInternalErr(fmt.Errorf(msg, args...))
}

/*
replaceQueryType - sets definition query type to a current root type.
Helps to do a normalization of the upstream query for a nested datasource.
Skips replace when:
1. datasource is not nested;
2. federation is enabled;
3. query type contains an operation field;

Example transformation:
Original schema definition:

	type Query {
		serviceOne(serviceOneArg: String): ServiceOneResponse
		serviceTwo(serviceTwoArg: Boolean): ServiceTwoResponse
	}

	type ServiceOneResponse {
		fieldOne: String!
		countries: [Country!]! # nested datasource without explicit field path
	}

	type ServiceTwoResponse {
		fieldTwo: String
		serviceOneField: String
		serviceOneResponse: ServiceOneResponse # nested datasource with implicit field path "serviceOne"
	}

	type Country {
		name: String!
	}

`serviceOneResponse` field of a `ServiceTwoResponse` is nested but has a field path that exists on the Query type
- In this case definition will not be modified

`countries` field of a `ServiceOneResponse` is nested and not present on the Query type
- In this case query type of definition will be replaced with a `ServiceOneResponse`

Modified schema definition:

	schema {
	   query: ServiceOneResponse
	}

	type ServiceOneResponse {
	   fieldOne: String!
	   countries: [Country!]!
	}

	type ServiceTwoResponse {
	   fieldTwo: String
	   serviceOneField: String
	   serviceOneResponse: ServiceOneResponse
	}

	type Country {
	   name: String!
	}

Refer to pkg/engine/datasource/graphql_datasource/graphql_datasource_test.go:632
Case name: TestGraphQLDataSource/nested_graphql_engines

If we didn't do this transformation, the normalization would fail because it's not possible
to traverse the AST as there's a mismatch between the upstream Operation and the schema.

If the nested Query can be rewritten so that it's a valid Query against the existing schema, fine.
However, when rewriting the nested Query onto the schema's Query type,
it might be the case that no FieldDefinition exists for the rewritten root field.
In that case, we transform the schema so that normalization and printing of the upstream Query succeeds.
*/
func (p *Planner) replaceQueryType(definition *ast.Document) {
	if !p.isNested {
		return
	}

	queryTypeName := definition.Index.QueryTypeName
	queryNode, exists := definition.Index.FirstNodeByNameBytes(queryTypeName)
	if !exists || queryNode.Kind != ast.NodeKindObjectTypeDefinition {
		return
	}

	// check that query type has rootFieldName within its fields
	hasField := definition.FieldDefinitionsContainField(definition.ObjectTypeDefinitions[queryNode.Ref].FieldsDefinition.Refs, []byte(p.rootFieldName))
	if hasField {
		return
	}

	definition.RemoveObjectTypeDefinition(definition.Index.QueryTypeName)
	definition.ReplaceRootOperationTypeDefinition(p.rootTypeName, ast.OperationTypeQuery)
}

// normalizeOperation - normalizes operation against definition.
func (p *Planner) normalizeOperation(operation, definition *ast.Document, report *operationreport.Report) (ok bool) {

	report.Reset()
	normalizer := astnormalization.NewWithOpts(
		astnormalization.WithExtractVariables(),
		astnormalization.WithRemoveFragmentDefinitions(),
		astnormalization.WithRemoveUnusedVariables(),
	)
	normalizer.NormalizeOperation(operation, definition, report)

	return !report.HasErrors()
}

// addField - add a field to an upstream operation
func (p *Planner) addField(ref int) {
	fieldName := p.visitor.Operation.FieldNameString(ref)

	/*alias := ast.Alias{
		IsDefined: p.visitor.Operation.FieldAliasIsDefined(ref),
	}

	if alias.IsDefined {
		aliasBytes := p.visitor.Operation.FieldAliasBytes(ref)
		alias.Name = p.upstreamOperation.Input.AppendInputBytes(aliasBytes)
	}*/

	typeName := p.visitor.Walker.EnclosingTypeDefinition.NameString(p.visitor.Definition)
	for i := range p.visitor.Config.Fields {
		isDesiredField := p.visitor.Config.Fields[i].TypeName == typeName &&
			p.visitor.Config.Fields[i].FieldName == fieldName

		// chech that we are on a desired field and field path contains a single element - mapping is plain
		if isDesiredField && len(p.visitor.Config.Fields[i].Path) == 1 {
			// define alias when mapping path differs from fieldName and no alias has been defined
			/*if p.visitor.Config.Fields[i].Path[0] != fieldName && !alias.IsDefined {
				alias.IsDefined = true
				aliasBytes := p.visitor.Operation.FieldNameBytes(ref)
				alias.Name = p.upstreamOperation.Input.AppendInputBytes(aliasBytes)
			}*/

			// override fieldName with mapping path value
			fieldName = p.visitor.Config.Fields[i].Path[0]

			// when provided field is a root type field save new field name
			if ref == p.rootFieldRef {
				p.rootFieldName = fieldName
			}

			break
		}
	}

	field := p.upstreamOperation.AddField(ast.Field{
		Name: p.upstreamOperation.Input.AppendInputString(fieldName),
		//Alias: alias,
	})

	selection := ast.Selection{
		Kind: ast.SelectionKindField,
		Ref:  field.Ref,
	}

	p.upstreamOperation.AddSelection(p.nodes[len(p.nodes)-1].Ref, selection)
	p.nodes = append(p.nodes, field)
}

type Factory struct {
	Client        *http.Client
	engineFactory LazyEngineFactory
	Debug         bool
	Log           abstractlogger.Logger
}

func (f *Factory) WithHTTPClient(client *http.Client) *Factory {
	return &Factory{
		Client:        client,
		engineFactory: f.engineFactory,
		Debug:         f.Debug,
		Log:           f.Log,
	}
}

func (f *Factory) Planner(ctx context.Context) plan.DataSourcePlanner {
	f.engineFactory.closer = ctx.Done()
	return &Planner{
		client:        f.Client,
		engineFactory: &f.engineFactory,
		debug:         f.Debug,
		log:           f.Log,
	}
}

type LazyEngineFactory struct {
	closer  <-chan struct{}
	engines map[string]*LazyEngine
}

func (f *LazyEngineFactory) Engine(prismaSchema, wundergraphDir string, closeTimeoutSeconds int64) *LazyEngine {
	if f.engines == nil {
		f.engines = map[string]*LazyEngine{}
	}
	engine, exists := f.engines[prismaSchema]
	if exists {
		return engine
	}
	engine = &LazyEngine{
		m:                   &sync.RWMutex{},
		prismaSchema:        prismaSchema,
		wundergraphDir:      wundergraphDir,
		closeTimeoutSeconds: closeTimeoutSeconds,
	}
	go engine.Start(f.closer)
	runtime.SetFinalizer(engine, finalizeEngine)
	f.engines[prismaSchema] = engine
	return engine
}

func finalizeEngine(e *LazyEngine) {
	if e.engine != nil {
		panic("engine must be closed and set to nil before handing it over to gc")
	}
}

type LazyEngine struct {
	m *sync.RWMutex

	prismaSchema        string
	wundergraphDir      string
	closeTimeoutSeconds int64

	engine HybridEngine
	closed bool

	requestWasProcessed chan struct{}
}

func (e *LazyEngine) Start(closer <-chan struct{}) {

	e.requestWasProcessed = make(chan struct{})

	var stopEngine <-chan time.Time

	for {
		select {
		case <-e.requestWasProcessed:
			stopEngine = time.After(time.Second * time.Duration(e.closeTimeoutSeconds))
		case <-stopEngine:
			e.m.Lock()
			if e.engine != nil {
				e.engine.Close()
				e.engine = nil
			}
			e.m.Unlock()
		case <-closer:
			e.m.Lock()
			if e.engine != nil {
				e.engine.Close()
				e.engine = nil
			}
			e.closed = true
			e.m.Unlock()
			return
		}
	}
}

func (e *LazyEngine) execute(ctx context.Context, request []byte, out io.Writer) error {
	defer e.sendRequestProcessed()
	return e.engine.Execute(ctx, request, out)
}

func (e *LazyEngine) sendRequestProcessed() {
	e.requestWasProcessed <- struct{}{}
}

func (e *LazyEngine) Execute(ctx context.Context, request []byte, out io.Writer) error {
	e.m.RLock()
	if e.closed {
		e.m.RUnlock()
		return fmt.Errorf("engine closed")
	}
	if e.engine == nil {
		e.m.RUnlock()
		return e.initEngineAndExecute(ctx, request, out)
	}
	err := e.execute(ctx, request, out)
	e.m.RUnlock()
	return err
}

func (e *LazyEngine) initEngineAndExecute(ctx context.Context, request []byte, out io.Writer) error {
	e.m.Lock()
	defer e.m.Unlock()
	var err error
	e.engine, err = NewHybridEngine(e.prismaSchema, e.wundergraphDir, abstractlogger.NoopLogger)
	if err != nil {
		return err
	}
	err = e.engine.WaitUntilReady(ctx)
	if err != nil {
		return err
	}
	return e.execute(ctx, request, out)
}

type Source struct {
	engine *LazyEngine
	debug  bool
	log    abstractlogger.Logger
}

// {"query":"{findFirstusers(where: {name: {contains: null}}){name id updatedat}}","variables":{}}

func (s *Source) unNullRequest(request []byte) []byte {
	if end := bytes.Index(request, []byte(": null")); end != -1 {
		start1 := bytes.LastIndex(request[:end], []byte("{"))
		start2 := bytes.LastIndex(request[:end], []byte("("))
		start3 := bytes.LastIndex(request[:end], []byte(","))
		start4 := bytes.LastIndex(request[:end], []byte(" "))
		start := max(start1, start2, start3, start4)
		request = append(request[:start+1], request[end+6:]...)
		return s.unNullRequest(request)
	}
	return request
}

// max returns the highest of its arguments:
func max(start1 int, start2 int, start3 int, start4 int) int {
	if start1 > start2 {
		if start1 > start3 {
			if start1 > start4 {
				return start1
			}
			return start4
		}
		if start3 > start4 {
			return start3
		}
		return start4
	}
	if start2 > start3 {
		if start2 > start4 {
			return start2
		}
		return start4
	}
	if start3 > start4 {
		return start3
	}
	return start4
}

func (s *Source) Load(ctx context.Context, input []byte, w io.Writer) (err error) {
	request, _ := jsonparser.Set(input, []byte("{}"), "variables")
	request = s.unNullRequest(request)
	buf := pool.GetBytesBuffer()
	defer pool.PutBytesBuffer(buf)
	ctx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()
	for {
		if s.debug {
			s.log.Debug("database.Source.Execute",
				abstractlogger.ByteString("request", request),
			)
		}
		err = s.engine.Execute(ctx, request, buf)
		if err != nil {
			if s.debug {
				s.log.Debug("database.Source.Execute.Error",
					abstractlogger.ByteString("request", request),
					abstractlogger.Error(err),
				)
			}
			if ctx.Err() != nil {
				if s.debug {
					s.log.Debug("database.Source.Execute.Deadline Exceeded")
				}
				return err
			}
			err = nil
			time.Sleep(time.Millisecond * 500)
			buf.Reset()
			if s.debug {
				s.log.Debug("database.Source.Execute.Retry.after.Error")
			}
			continue
		}
		break
	}

	if s.debug {
		s.log.Debug("database.Source.Execute.Succeed",
			abstractlogger.ByteString("request", request),
			abstractlogger.String("response", buf.String()),
		)
	}
	_, err = buf.WriteTo(w)
	return
}
