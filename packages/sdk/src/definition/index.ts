import { configuration } from '../graphql/configuration';
import {
	buildClientSchema,
	buildSchema,
	GraphQLSchema,
	introspectionFromSchema,
	parse,
	print,
	printSchema,
} from 'graphql';
import * as fs from 'fs';
import { openApiSpecificationToRESTApiObject } from '../v2openapi';
import { renameTypeFields, renameTypes } from '../graphql/renametypes';
import {
	ArgumentSource,
	ConfigurationVariable,
	DataSourceKind,
	DirectiveConfiguration,
	FetchConfiguration,
	FieldConfiguration,
	GraphQLDataSourceHooksConfiguration,
	MTLSConfiguration,
	SigningMethod,
	SingleTypeField,
	StatusCodeTypeMapping,
	TypeConfiguration,
	TypeField,
	UpstreamAuthentication,
	UpstreamAuthenticationKind,
} from '@wundergraph/protobuf';
import path from 'path';
import { DatabaseSchema, introspectPrismaDatabaseWithRetries } from '../db/introspection';
import {
	applyNameSpaceToFieldConfigurations,
	applyNameSpaceToGraphQLSchema,
	applyNameSpaceToTypeFields,
	generateTypeConfigurationsForNamespace,
} from './namespacing';
import { introspectWithCache } from './introspection-cache';
import { InputVariable, mapInputVariable } from '../configure/variables';
import { introspectGraphql } from './graphql-introspection';
import { introspectFederation } from './federation-introspection';
import { IGraphqlIntrospectionHeadersBuilder, IHeadersBuilder } from './headers-builder';

// Use UPPERCASE for environment variables
export const WG_DATA_SOURCE_POLLING_MODE = process.env['WG_DATA_SOURCE_POLLING_MODE'] === 'true';
export const WG_ENABLE_INTROSPECTION_CACHE = process.env['WG_ENABLE_INTROSPECTION_CACHE'] === 'true';
// Only use the instrospection cache, return an error when hitting the network
export const WG_ENABLE_INTROSPECTION_OFFLINE = process.env['WG_ENABLE_INTROSPECTION_OFFLINE'] === 'true';

export interface ApplicationConfig {
	name: string;
	apis: Promise<Api<any>>[];
}

export class Application {
	constructor(config: ApplicationConfig) {
		this.name = config.name;
		this.apis = config.apis;
	}

	name: string;
	apis: Promise<Api<any>>[];
}

export interface RenameType {
	from: string;
	to: string;
}

export interface RenameTypes {
	renameTypes: (rename: RenameType[]) => void;
}

export interface RenameTypeField {
	typeName: string;
	fromFieldName: string;
	toFieldName: string;
}

export interface RenameTypeFields {
	renameTypeFields: (rename: RenameTypeField[]) => void;
}

export type ApiType = GraphQLApiCustom | RESTApiCustom | DatabaseApiCustom;

export class Api<T = ApiType> implements RenameTypes, RenameTypeFields {
	constructor(
		schema: string,
		dataSources: DataSource<T>[],
		fields: FieldConfiguration[],
		types: TypeConfiguration[],
		interpolateVariableDefinitionAsJSON: string[]
	) {
		this.Schema = schema;
		this.DataSources = dataSources;
		this.Fields = fields;
		this.Types = types;
		this.interpolateVariableDefinitionAsJSON = interpolateVariableDefinitionAsJSON;
	}

	DefaultFlushInterval: number = 500;
	Schema: string;
	DataSources: DataSource<T>[];
	Fields: FieldConfiguration[];
	Types: TypeConfiguration[];
	interpolateVariableDefinitionAsJSON: string[];

	renameTypes(rename: RenameType[]): void {
		this.Schema = renameTypes(this.Schema, rename);
		this.DataSources = this.DataSources.map((d) => {
			return {
				...d,
				RootNodes: typeFieldsRenameType(d.RootNodes, rename),
				ChildNodes: typeFieldsRenameType(d.ChildNodes, rename),
			};
		});
		this.Fields = this.Fields.map((field) => {
			const re = rename.find((r) => r.from === field.typeName);
			return {
				...field,
				typeName: re !== undefined ? re.to : field.typeName,
			};
		});
	}

	renameTypeFields(rename: RenameTypeField[]): void {
		this.Schema = renameTypeFields(this.Schema, rename);
		this.DataSources = this.DataSources.map((d) => {
			return {
				...d,
				RootNodes: typeFieldsRenameTypeField(d.RootNodes, rename),
				ChildNodes: typeFieldsRenameTypeField(d.ChildNodes, rename),
			};
		});
		this.Fields = this.Fields.map((field) => {
			const re = rename.find((re) => re.typeName === field.typeName && re.fromFieldName === field.fieldName);
			if (re !== undefined) {
				return {
					...field,
					fieldName: re.toFieldName,
					path: field.path.map((item) => (item === field.fieldName ? re.toFieldName : item)),
				};
			}
			const sameTypeRenameFields = rename.filter((re) => re.typeName === field.typeName);
			return {
				...field,
				requiresFields: field.requiresFields.map((f) => {
					const re = sameTypeRenameFields.find((sameTypeField) => sameTypeField.fromFieldName === f);
					if (re !== undefined) {
						return re.toFieldName;
					}
					return f;
				}),
				argumentsConfiguration: field.argumentsConfiguration.map((arg) => {
					if (arg.sourceType === ArgumentSource.OBJECT_FIELD) {
						return {
							...arg,
							sourcePath: arg.sourcePath.map((item) => {
								const re = sameTypeRenameFields.find((sameTypeField) => sameTypeField.fromFieldName === item);
								if (re !== undefined) {
									return re.toFieldName;
								}
								return item;
							}),
						};
					}
					return arg;
				}),
			};
		});
	}
}

const typeFieldsRenameType = (fields: TypeField[], rename: RenameType[]): TypeField[] => {
	return fields.map((node) => {
		const re = rename.find((r) => r.from === node.typeName);
		return {
			...node,
			typeName: re !== undefined ? re.to : node.typeName,
		};
	});
};

const typeFieldsRenameTypeField = (fields: TypeField[], rename: RenameTypeField[]): TypeField[] => {
	return fields.map((node) => {
		return {
			...node,
			fieldNames: node.fieldNames.map((field) => {
				const re = rename.find((re) => re.typeName === node.typeName && re.fromFieldName === field);
				if (re !== undefined) {
					return re.toFieldName;
				}
				return field;
			}),
		};
	});
};

export const createMockApi = async (sdl: string, apiNamespace?: string): Promise<Api<any>> => {
	const schema = print(parse(sdl));
	return new GraphQLApi(applyNameSpaceToGraphQLSchema(schema, [], apiNamespace), [], [], [], []);
};

export class GraphQLApi extends Api<GraphQLApiCustom> {}

export class RESTApi extends Api<RESTApiCustom> {}

export class PostgresqlApi extends Api<DatabaseApiCustom> {}

export class MySQLApi extends Api<DatabaseApiCustom> {}

export class PlanetscaleApi extends Api<DatabaseApiCustom> {}

export class SQLiteApi extends Api<DatabaseApiCustom> {}

export class SQLServerApi extends Api<DatabaseApiCustom> {}

export class MongoDBApi extends Api<DatabaseApiCustom> {}

export interface DataSource<Custom = unknown> {
	Id?: string;
	Kind: DataSourceKind;
	RootNodes: TypeField[];
	ChildNodes: TypeField[];
	Custom: Custom;
	Directives: DirectiveConfiguration[];
	RequestTimeoutSeconds: number;
}

interface GraphQLIntrospectionOptions {
	// loadSchemaFromString allows you to skip the introspection process and load the GraphQL Schema from a string instead
	// this way, you can import a GraphQL Schema file or load the Schema in more flexible ways than relying on sending a GraphQL Introspection Query
	loadSchemaFromString?: string | (() => string);
	customFloatScalars?: string[];
	customIntScalars?: string[];
	// switching internal to true will mark the origin as an internal GraphQL API
	// this will forward the original request and user info to the internal upstream
	// so that the request context can be enriched
	internal?: boolean;
	skipRenameRootFields?: string[];
	// the schemaExtension field is used to extend the generated GraphQL schema with additional types and fields
	// this is useful for specifying type definitions for JSON objects
	schemaExtension?: string;
	replaceCustomScalarTypeFields?: ReplaceCustomScalarTypeFieldConfiguration[];
}

export interface GraphQLIntrospection extends GraphQLUpstream, GraphQLIntrospectionOptions {
	isFederation?: boolean;
}

export interface GraphQLFederationUpstream extends Omit<Omit<GraphQLUpstream, 'introspection'>, 'apiNamespace'> {
	name?: string;
	loadSchemaFromString?: GraphQLIntrospectionOptions['loadSchemaFromString'];
	introspection?: GraphqlIntrospectionHeaders;
}

export interface GraphQLFederationIntrospection extends IntrospectionConfiguration {
	upstreams: GraphQLFederationUpstream[];
	apiNamespace?: string;
}

export interface ReplaceCustomScalarTypeFieldConfiguration {
	entityName: string;
	fieldName: string;
	inputTypeReplacement?: string;
	responseTypeReplacement: string;
}

export interface DatabaseIntrospection extends IntrospectionConfiguration {
	databaseURL: InputVariable;
	apiNamespace?: string;
	// the schemaExtension field is used to extend the generated GraphQL schema with additional types and fields
	// this is useful for specifying type definitions for JSON objects
	schemaExtension?: string;
	replaceCustomScalarTypeFields?: ReplaceCustomScalarTypeFieldConfiguration[];
}

export interface IntrospectionConfiguration {
	// id is the unique identifier for the data source
	id?: string;
	/**
	 * Timeout for network requests originated by this data source, in seconds.
	 *
	 * @remarks
	 * See {@link NodeOptions| the NodeOptions type} for more details.
	 *
	 * @defaultValue Use the default timeout for this node.
	 */
	requestTimeoutSeconds?: number;
	introspection?: {
		disableCache?: boolean;
		pollingIntervalSeconds?: number;
	};
}

export interface HTTPUpstream extends IntrospectionConfiguration {
	apiNamespace?: string;
	headers?: (builder: IHeadersBuilder) => IHeadersBuilder;
	authentication?: HTTPUpstreamAuthentication;
	mTLS?: HTTPmTlsConfiguration;
}

export type HTTPmTlsConfiguration = {
	/**
	 * 	Private-key or environment variable name that stores the key
	 */
	key: InputVariable;
	/**
	 * 	X.509 (TLS/HTTPS) or environment variable name that stores the certificate
	 */
	cert: InputVariable;
	/**
	 * InsecureSkipVerify controls whether a client verifies the server's certificate chain and host name
	 * If InsecureSkipVerify is true, crypto/tls accepts any certificate presented by the server and any host name in that certificate.
	 * In this mode, TLS is susceptible to machine-in-the-middle attacks unless custom verification is used.
	 * This should be used only for testing
	 */
	insecureSkipVerify: boolean;
};

export type HTTPUpstreamAuthentication = JWTAuthentication | JWTAuthenticationWithAccessTokenExchange;

export interface JWTAuthentication {
	kind: 'jwt';
	secret: InputVariable;
	signingMethod: JWTSigningMethod;
}

export interface JWTAuthenticationWithAccessTokenExchange {
	kind: 'jwt_with_access_token_exchange';
	secret: InputVariable;
	signingMethod: JWTSigningMethod;
	accessTokenExchangeEndpoint: InputVariable;
}

export type JWTSigningMethod = 'HS256';

export interface GraphqlIntrospectionHeaders {
	headers?: (builder: IGraphqlIntrospectionHeadersBuilder) => IGraphqlIntrospectionHeadersBuilder;
}

export interface GraphQLUpstream extends HTTPUpstream {
	url: InputVariable;
	baseUrl?: InputVariable;
	path?: InputVariable;
	subscriptionsURL?: InputVariable;
	subscriptionsUseSSE?: boolean;
	introspection?: HTTPUpstream['introspection'] & GraphqlIntrospectionHeaders;
}

export interface OpenAPIIntrospectionFile {
	kind: 'file';
	filePath: string;
}

export interface OpenAPIIntrospectionString {
	kind: 'string';
	openAPISpec: string;
}

export interface OpenAPIIntrospectionObject {
	kind: 'object';
	openAPIObject: {};
}

export type OpenAPIIntrospectionSource =
	| OpenAPIIntrospectionFile
	| OpenAPIIntrospectionString
	| OpenAPIIntrospectionObject;

export interface OpenAPIIntrospection extends HTTPUpstream {
	source: OpenAPIIntrospectionSource;
	// statusCodeUnions set to true will make all responses return a union type of all possible response objects,
	// mapped by status code
	// by default, only the status 200 response is mapped, which keeps the GraphQL API flat
	// by enabling statusCodeUnions, you have to unwrap the response union via fragments for each response
	statusCodeUnions?: boolean;
	baseURL?: string;
	// the schemaExtension field is used to extend the generated GraphQL schema with additional types and fields
	// this is useful for specifying type definitions for JSON objects
	schemaExtension?: string;
	replaceCustomScalarTypeFields?: ReplaceCustomScalarTypeFieldConfiguration[];
}

export interface StaticApiCustom {
	data: ConfigurationVariable;
}

export interface RESTApiCustom {
	Fetch: FetchConfiguration;
	Subscription: SubscriptionConfiguration;
	DefaultTypeName: string;
	StatusCodeTypeMappings: StatusCodeTypeMapping[];
}

export interface DatabaseApiCustom {
	prisma_schema: string;
	graphql_schema: string;
	databaseURL: ConfigurationVariable;
	jsonTypeFields: SingleTypeField[];
	jsonInputVariables: string[];
}

export interface SubscriptionConfiguration {
	Enabled: boolean;
	PollingIntervalMillis?: number;
	SkipPublishSameResponse?: boolean;
}

export interface GraphQLApiCustom {
	Federation: {
		Enabled: boolean;
		ServiceSDL: string;
	};
	Fetch: FetchConfiguration;
	Subscription: {
		Enabled: boolean;
		URL: ConfigurationVariable;
		UseSSE: boolean;
	};
	UpstreamSchema: string;
	HooksConfiguration: GraphQLDataSourceHooksConfiguration;
}

export interface GraphQLServerConfiguration extends Omit<GraphQLIntrospection, 'loadSchemaFromString'> {
	schema: GraphQLSchema | Promise<GraphQLSchema>;
}

const databaseSchemaToKind = (schema: DatabaseSchema): DataSourceKind => {
	switch (schema) {
		case 'planetscale':
			return DataSourceKind.MYSQL;
		case 'mysql':
			return DataSourceKind.MYSQL;
		case 'postgresql':
			return DataSourceKind.POSTGRESQL;
		case 'sqlite':
			return DataSourceKind.SQLITE;
		case 'sqlserver':
			return DataSourceKind.SQLSERVER;
		case 'mongodb':
			return DataSourceKind.MONGODB;
		default:
			throw new Error(`databaseSchemaToKind not implemented for: ${schema}`);
	}
};

const introspectDatabase = async (
	introspection: DatabaseIntrospection,
	databaseSchema: DatabaseSchema,
	maxRetries: number
) => {
	const {
		success,
		message,
		graphql_schema,
		prisma_schema,
		interpolateVariableDefinitionAsJSON,
		jsonTypeFields,
		jsonResponseFields,
	} = await introspectPrismaDatabaseWithRetries(introspection, databaseSchema, maxRetries);
	if (!success) {
		return Promise.reject(message);
	}
	const schemaDocumentNode = parse(graphql_schema);
	const schema = print(schemaDocumentNode);
	const { RootNodes, ChildNodes, Fields } = configuration(schemaDocumentNode);
	const jsonFields = [...jsonTypeFields, ...jsonResponseFields];
	jsonFields.forEach((field) => {
		const fieldConfig = Fields.find((f) => f.typeName == field.typeName && f.fieldName == field.fieldName);
		if (fieldConfig) {
			fieldConfig.unescapeResponseJson = true;
		} else {
			Fields.push({
				fieldName: field.fieldName,
				typeName: field.typeName,
				unescapeResponseJson: true,
				argumentsConfiguration: [],
				path: [],
				requiresFields: [],
				disableDefaultFieldMapping: false,
			});
		}
	});
	const graphQLSchema = buildSchema(schema);
	const dataSource: DataSource<DatabaseApiCustom> = {
		Kind: databaseSchemaToKind(databaseSchema),
		RootNodes: applyNameSpaceToTypeFields(RootNodes, graphQLSchema, introspection.apiNamespace),
		ChildNodes: applyNameSpaceToTypeFields(ChildNodes, graphQLSchema, introspection.apiNamespace),
		Custom: {
			prisma_schema: prisma_schema,
			databaseURL: mapInputVariable(introspection.databaseURL),
			graphql_schema: schema,
			jsonTypeFields: applyNameSpaceToSingleTypeFields(jsonTypeFields, introspection.apiNamespace),
			jsonInputVariables: applyNameSpaceToTypeNames(interpolateVariableDefinitionAsJSON, introspection.apiNamespace),
		},
		Directives: [],
		RequestTimeoutSeconds: introspection.requestTimeoutSeconds ?? 0,
	};
	const dataSources: DataSource<DatabaseApiCustom>[] = [];
	dataSource.RootNodes.forEach((rootNode) => {
		rootNode.fieldNames.forEach((field) => {
			dataSources.push({
				...Object.assign({}, dataSource),
				RootNodes: [
					{
						typeName: rootNode.typeName,
						fieldNames: [field],
					},
				],
			});
		});
	});
	return {
		schema: applyNameSpaceToGraphQLSchema(schema, [], introspection.apiNamespace),
		dataSources: dataSources,
		fields: applyNameSpaceToFieldConfigurations(Fields, graphQLSchema, [], introspection.apiNamespace),
		types: generateTypeConfigurationsForNamespace(schema, introspection.apiNamespace),
		interpolateVariableDefinitionAsJSON: applyNameSpaceToTypeNames(
			interpolateVariableDefinitionAsJSON,
			introspection.apiNamespace
		),
	};
};

const applyNameSpaceToSingleTypeFields = (typeFields: SingleTypeField[], namespace?: string): SingleTypeField[] => {
	if (!namespace) {
		return typeFields;
	}
	return typeFields.map((typeField) => ({
		...typeField,
		typeName: `${namespace}_${typeField.typeName}`,
	}));
};

const applyNameSpaceToTypeNames = (typeNames: string[], namespace?: string): string[] => {
	if (!namespace) {
		return typeNames;
	}
	return typeNames.map((typeName) => {
		return `${namespace}_${typeName}`;
	});
};

export const introspectGraphqlServer = async (introspection: GraphQLServerConfiguration): Promise<GraphQLApi> => {
	const { schema, ...rest } = introspection;
	const resolvedSchema = (await schema) as GraphQLSchema;

	return introspect.graphql({
		...rest,
		internal: true,
		loadSchemaFromString: () => printSchema(buildClientSchema(introspectionFromSchema(resolvedSchema))),
	});
};

export const introspect = {
	graphql: introspectGraphql,
	postgresql: async (introspection: DatabaseIntrospection): Promise<PostgresqlApi> =>
		introspectWithCache(introspection, async (introspection: DatabaseIntrospection): Promise<PostgresqlApi> => {
			const { schema, fields, types, dataSources, interpolateVariableDefinitionAsJSON } = await introspectDatabase(
				introspection,
				'postgresql',
				5
			);
			return new PostgresqlApi(schema, dataSources, fields, types, interpolateVariableDefinitionAsJSON);
		}),
	mysql: async (introspection: DatabaseIntrospection): Promise<MySQLApi> =>
		introspectWithCache(introspection, async (introspection: DatabaseIntrospection): Promise<MySQLApi> => {
			const { schema, fields, types, dataSources, interpolateVariableDefinitionAsJSON } = await introspectDatabase(
				introspection,
				'mysql',
				5
			);
			return new MySQLApi(schema, dataSources, fields, types, interpolateVariableDefinitionAsJSON);
		}),
	planetscale: async (introspection: DatabaseIntrospection): Promise<PlanetscaleApi> =>
		introspectWithCache(introspection, async (introspection: DatabaseIntrospection): Promise<PlanetscaleApi> => {
			const { schema, fields, types, dataSources, interpolateVariableDefinitionAsJSON } = await introspectDatabase(
				introspection,
				'planetscale',
				5
			);
			return new PlanetscaleApi(schema, dataSources, fields, types, interpolateVariableDefinitionAsJSON);
		}),
	sqlite: async (introspection: DatabaseIntrospection): Promise<SQLiteApi> =>
		introspectWithCache(introspection, async (introspection: DatabaseIntrospection): Promise<SQLiteApi> => {
			const { schema, fields, types, dataSources, interpolateVariableDefinitionAsJSON } = await introspectDatabase(
				introspection,
				'sqlite',
				5
			);
			return new SQLiteApi(schema, dataSources, fields, types, interpolateVariableDefinitionAsJSON);
		}),
	sqlserver: async (introspection: DatabaseIntrospection): Promise<SQLServerApi> =>
		introspectWithCache(introspection, async (introspection: DatabaseIntrospection): Promise<SQLServerApi> => {
			const { schema, fields, types, dataSources, interpolateVariableDefinitionAsJSON } = await introspectDatabase(
				introspection,
				'sqlserver',
				5
			);
			return new SQLServerApi(schema, dataSources, fields, types, interpolateVariableDefinitionAsJSON);
		}),
	mongodb: async (introspection: DatabaseIntrospection): Promise<MongoDBApi> =>
		introspectWithCache(introspection, async (introspection: DatabaseIntrospection): Promise<MongoDBApi> => {
			const { schema, fields, types, dataSources, interpolateVariableDefinitionAsJSON } = await introspectDatabase(
				introspection,
				'mongodb',
				5
			);
			return new MongoDBApi(schema, dataSources, fields, types, interpolateVariableDefinitionAsJSON);
		}),
	federation: introspectFederation,
	openApi: async (introspection: OpenAPIIntrospection): Promise<RESTApi> => {
		const generator = async (introspection: OpenAPIIntrospection): Promise<RESTApi> => {
			const spec = loadOpenApi(introspection);
			return await openApiSpecificationToRESTApiObject(spec, introspection);
		};
		// If the source is a file we have all data required to perform the instrospection
		// locally, which is also fast. Skip the cache in this case, so changes to the file
		// are picked up immediately without requiring a cache flush.
		if (introspection.source.kind === 'file') {
			return generator(introspection);
		}
		return introspectWithCache(introspection, generator);
	},
};

export const buildUpstreamAuthentication = (upstream: HTTPUpstream): UpstreamAuthentication | undefined => {
	if (upstream.authentication === undefined) {
		return undefined;
	}
	return {
		kind: upstreamAuthenticationKind(upstream.authentication.kind),
		jwtConfig:
			upstream.authentication.kind === 'jwt'
				? {
						secret: mapInputVariable(upstream.authentication.secret),
						signingMethod: upstreamAuthenticationSigningMethod(upstream.authentication.signingMethod),
				  }
				: undefined,
		jwtWithAccessTokenExchangeConfig:
			upstream.authentication.kind === 'jwt_with_access_token_exchange'
				? {
						accessTokenExchangeEndpoint: mapInputVariable(upstream.authentication.accessTokenExchangeEndpoint),
						secret: mapInputVariable(upstream.authentication.secret),
						signingMethod: upstreamAuthenticationSigningMethod(upstream.authentication.signingMethod),
				  }
				: undefined,
	};
};

export const buildMTLSConfiguration = (upstream: HTTPUpstream): MTLSConfiguration | undefined => {
	if (upstream.mTLS === undefined) {
		return undefined;
	}
	return {
		key: mapInputVariable(upstream.mTLS?.key || ''),
		cert: mapInputVariable(upstream.mTLS?.cert || ''),
		insecureSkipVerify: upstream.mTLS?.insecureSkipVerify || false,
	};
};

const upstreamAuthenticationSigningMethod = (signingMethod: JWTSigningMethod): SigningMethod => {
	switch (signingMethod) {
		case 'HS256':
			return SigningMethod.SigningMethodHS256;
		default:
			throw new Error(`JWT signing method unsupported: ${signingMethod}`);
	}
};

const upstreamAuthenticationKind = (kind: HTTPUpstreamAuthentication['kind']): UpstreamAuthenticationKind => {
	switch (kind) {
		case 'jwt':
			return UpstreamAuthenticationKind.UpstreamAuthenticationJWT;
		case 'jwt_with_access_token_exchange':
			return UpstreamAuthenticationKind.UpstreamAuthenticationJWTWithAccessTokenExchange;
		default:
			throw new Error(`upstreamAuthenticationKind, unsupported kind: ${kind}`);
	}
};

const loadOpenApi = (introspection: OpenAPIIntrospection): string => {
	switch (introspection.source.kind) {
		case 'file':
			const filePath = path.resolve(process.cwd(), introspection.source.filePath);
			return fs.readFileSync(filePath).toString();
		case 'object':
			return JSON.stringify(introspection.source.openAPIObject);
		case 'string':
			return introspection.source.openAPISpec;
		default:
			return '';
	}
};
