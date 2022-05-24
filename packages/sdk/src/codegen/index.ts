import { ResolvedWunderGraphConfig } from '../configure';
import path from 'path';
import * as fs from 'fs';
import { JSONSchema7 as JSONSchema } from 'json-schema';

export interface TemplateOutputFile {
	path: string;
	content: string;
	doNotEditHeader: boolean;
}

export interface Template {
	generate: (config: ResolvedWunderGraphConfig) => Promise<TemplateOutputFile[]>;
	dependencies?: () => Template[];
}

export interface CodeGenConfig {
	basePath: string;
	wunderGraphConfig: ResolvedWunderGraphConfig;
	templates: Template[];
}

export interface CodeGenOutWriter {
	writeFileSync: (path: string, content: string) => void;
}

class FileSystem implements CodeGenOutWriter {
	writeFileSync(path: string, content: string): void {
		ensurePath(path);
		fs.writeFileSync(path, content);
	}
}

const ensurePath = (filePath: string) => {
	const dir = path.dirname(filePath);
	if (!fs.existsSync(dir)) {
		fs.mkdirSync(dir, { recursive: true });
	}
};

const doNotEditHeader = '// Code generated by wunderctl. DO NOT EDIT.\n\n';

export const GenerateCode = async (config: CodeGenConfig, customOutWriter?: CodeGenOutWriter) => {
	config.templates
		.filter((tmpl) => tmpl !== undefined)
		.forEach((tmpl) => {
			if (!tmpl.dependencies) {
				return;
			}
			const deps = tmpl.dependencies();
			config.templates.push(...deps);
		});

	const dedupedTemplates: Template[] = [];

	config.templates
		.filter((tmpl) => tmpl !== undefined)
		.forEach((tmpl) => {
			const exists = dedupedTemplates.find((value) => value.constructor.name === tmpl.constructor.name) !== undefined;
			if (!exists) {
				dedupedTemplates.push(tmpl);
			}
		});

	const outWriter = customOutWriter || new FileSystem();
	const generators: Promise<TemplateOutputFile[]>[] = [];
	dedupedTemplates.forEach((template) => {
		generators.push(template.generate(config.wunderGraphConfig));
	});
	const resolved = await Promise.all(generators);
	const rawOutFiles: TemplateOutputFile[] = resolved.reduce((previousValue, currentValue) => [
		...previousValue,
		...currentValue,
	]);
	const outFiles = mergeTemplateOutput(rawOutFiles);
	outFiles.forEach((file) => {
		const content = file.doNotEditHeader ? doNotEditHeader + file.content : file.content;
		const outPath = path.join(config.basePath, file.path);
		outWriter.writeFileSync(outPath, content);
		console.log(`${new Date().toLocaleTimeString()}: ${outPath} updated`);
	});
};

export const mergeTemplateOutput = (outFiles: TemplateOutputFile[]): TemplateOutputFile[] => {
	const merged: TemplateOutputFile[] = [];
	outFiles.forEach((file) => {
		const existing = merged.find((out) => out.path === file.path);
		if (existing) {
			existing.content += '\n\n' + file.content;
		} else {
			merged.push(file);
		}
	});
	merged.forEach((file) => {
		while (file.content.search('\n\n\n') !== -1) {
			file.content = file.content.replace('\n\n\n', '\n\n');
		}
	});
	return merged;
};

export type VisitorCallBack = (name: string, isRequired: boolean, isArray: boolean) => void;
export type CustomTypeVisitorCallBack = (
	propertyName: string,
	typeName: string,
	isRequired: boolean,
	isArray: boolean
) => void;
export type StringVisitorCallBack = (
	name: string,
	isRequired: boolean,
	isArray: boolean,
	enumValues?: string[]
) => void;

export interface VisitorCallBacks {
	enter?: VisitorCallBack;
	leave?: VisitorCallBack;
}

export interface SchemaVisitor {
	root?: {
		enter?: () => void;
		leave?: () => void;
	};
	number?: VisitorCallBack;
	boolean?: VisitorCallBack;
	object?: VisitorCallBacks;
	string?: StringVisitorCallBack;
	array?: VisitorCallBacks;
	any?: VisitorCallBack;
	customType?: CustomTypeVisitorCallBack;
}

export const visitJSONSchema = (schema: JSONSchema, visitor: SchemaVisitor) => {
	visitor.root && visitor.root.enter && visitor.root.enter();
	visitProperties(schema, visitor);
	visitor.root && visitor.root.leave && visitor.root.leave();
};

const visitProperties = (schema: JSONSchema, visitor: SchemaVisitor) => {
	if (!schema.properties) {
		return;
	}
	Object.keys(schema.properties).forEach((key) => {
		const isRequired = (schema.required && schema.required.find((req) => req === key) !== undefined) || false;
		const propertySchema = schema.properties![key] as JSONSchema;
		visitSchema(propertySchema, visitor, key, isRequired, false);
	});
};

const visitSchema = (
	schema: JSONSchema,
	visitor: SchemaVisitor,
	propertyName: string,
	isRequired: boolean,
	isArray: boolean
) => {
	if (schema.$ref !== undefined) {
		const definitionName = schema.$ref.substring(schema.$ref.lastIndexOf('/') + 1);
		visitor.customType && visitor.customType(propertyName, definitionName, isRequired, isArray);
		return;
	}
	let schemaType: string | undefined;
	if (schema.type !== undefined && Array.isArray(schema.type)) {
		schemaType = schema.type.find((type) => type !== 'null') || '';
	} else {
		schemaType = schema.type || '';
	}
	switch (schemaType) {
		case 'number':
			visitor.number && visitor.number(propertyName, isRequired, isArray);
			break;
		case 'boolean':
			visitor.boolean && visitor.boolean(propertyName, isRequired, isArray);
			break;
		case 'object':
			visitor.object && visitor.object.enter && visitor.object.enter(propertyName, isRequired, isArray);
			visitProperties(schema, visitor);
			visitor.object && visitor.object.leave && visitor.object.leave(propertyName, isRequired, isArray);
			break;
		case 'string':
			visitor.string && visitor.string(propertyName, isRequired, isArray, schema.enum as string[]);
			break;
		case 'array':
			visitor.array && visitor.array.enter && visitor.array.enter(propertyName, isRequired, isArray);
			visitSchema(schema.items as JSONSchema, visitor, '', isRequired, true);
			visitor.array && visitor.array.leave && visitor.array.leave(propertyName, isRequired, isArray);
			break;
		case 'integer':
			visitor.number && visitor.number(propertyName, isRequired, isArray);
			break;
		default:
			visitor.any && visitor.any(propertyName, isRequired, isArray);
	}
};
