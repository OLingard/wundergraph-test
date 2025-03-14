import { handlebarTemplate } from './handlebar.template';
import Handlebars from 'handlebars';
import {
	Template,
	TemplateOutputFile,
	BaseTypeScriptDataModel,
	formatTypeScript,
	TypeScriptInputModels,
	TypeScriptResponseDataModels,
	TypeScriptResponseModels,
	ResolvedWunderGraphConfig,
	GraphQLOperation,
} from '@wundergraph/sdk';
import { modelImports } from '@wundergraph/sdk/internal';
import hash from 'object-hash';
import { OperationType } from '@wundergraph/protobuf';

export class NextJsTemplate implements Template {
	generate(config: ResolvedWunderGraphConfig): Promise<TemplateOutputFile[]> {
		const tmpl = Handlebars.compile(handlebarTemplate);
		const content = tmpl({
			baseURL: config.deployment.environment.baseUrl,
			sdkVersion: config.sdkVersion,
			applicationPath: config.deployment.path,
			applicationHash: hash(config).substring(0, 8),
			roleDefinitions: config.authentication.roles.map((role) => '"' + role + '"').join(' | '),
			modelImports: modelImports(config.application, false, true),
			queriesWithInput: config.application.Operations.filter(filterOperation(OperationType.QUERY, true)).map(
				mapOperation
			),
			queriesWithoutInput: config.application.Operations.filter(filterOperation(OperationType.QUERY, false)).map(
				mapOperation
			),
			liveQueriesWithInput: config.application.Operations.filter(filterOperation(OperationType.QUERY, true))
				.filter(isLiveQuery)
				.map(mapOperation),
			liveQueriesWithoutInput: config.application.Operations.filter(filterOperation(OperationType.QUERY, false))
				.filter(isLiveQuery)
				.map(mapOperation),
			mutationsWithInput: config.application.Operations.filter(filterOperation(OperationType.MUTATION, true)).map(
				mapOperation
			),
			mutationsWithoutInput: config.application.Operations.filter(filterOperation(OperationType.MUTATION, false)).map(
				mapOperation
			),
			subscriptionsWithInput: config.application.Operations.filter(
				filterOperation(OperationType.SUBSCRIPTION, true)
			).map(mapOperation),
			subscriptionsWithoutInput: config.application.Operations.filter(
				filterOperation(OperationType.SUBSCRIPTION, false)
			).map(mapOperation),
			hasAuthProviders: config.authentication.cookieBased.length !== 0,
			authProviders: config.authentication.cookieBased.map((provider) => provider.id),
			hasS3Providers: config.application.S3UploadProvider.length !== 0,
			s3Providers: config.application.S3UploadProvider.map((provider) => provider.name),
		});
		return Promise.resolve([
			{
				path: 'nextjs.ts',
				content: formatTypeScript(content),
				doNotEditHeader: true,
			},
		]);
	}

	dependencies(): Template[] {
		return [
			new TypeScriptInputModels(),
			new TypeScriptResponseModels(),
			new TypeScriptResponseDataModels(),
			new BaseTypeScriptDataModel(),
		];
	}
}

const filterOperation = (operationType: OperationType, withInput: boolean) => (operation: GraphQLOperation) => {
	return (
		operation.OperationType === operationType &&
		!operation.Internal &&
		(withInput
			? Object.keys(operation.VariablesSchema.properties || {}).length !== 0
			: Object.keys(operation.VariablesSchema.properties || {}).length === 0)
	);
};

const isLiveQuery = (operation: GraphQLOperation) => {
	return operation.OperationType === OperationType.QUERY && operation.LiveQuery?.enable === true;
};

const mapOperation = (operation: GraphQLOperation) => ({
	name: operation.Name,
	requiresAuthentication: operation.AuthenticationConfig.required,
});
