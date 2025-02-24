package instance

import (
	"github.com/cirruslabs/cirrus-cli/pkg/parser/instance/resources"
	"github.com/cirruslabs/cirrus-cli/pkg/parser/nameable"
	"github.com/cirruslabs/cirrus-cli/pkg/parser/node"
	"github.com/cirruslabs/cirrus-cli/pkg/parser/parseable"
	"github.com/cirruslabs/cirrus-cli/pkg/parser/parserkit"
	"github.com/cirruslabs/cirrus-cli/pkg/parser/schema"
	jsschema "github.com/lestrrat-go/jsschema"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"
	"sort"
	"strconv"
	"strings"
)

type ProtoInstance struct {
	proto *dynamicpb.Message

	parseable.DefaultParser
}

//nolint:gocognit,gocyclo,nestif // it's a parser, there is a lot of boilerplate
func NewProtoParser(
	desc protoreflect.MessageDescriptor,
	mergedEnv map[string]string,
	parserKit *parserkit.ParserKit,
) *ProtoInstance {
	instance := &ProtoInstance{
		proto: dynamicpb.NewMessage(desc),
	}

	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		fieldName := string(field.Name())

		// Empty for now
		fieldDescription := ""

		switch field.Kind() {
		case protoreflect.MessageKind:
			var messageSchema *jsschema.Schema

			switch {
			case field.IsMap():
				messageSchema = schema.Map(fieldDescription)
			case field.IsList():
				if fieldName == "additional_containers" {
					messageSchema = schema.ArrayOf(NewAdditionalContainer(nil, nil).Schema())
				} else {
					messageSchema = schema.ArrayOf(NewProtoParser(field.Message(), nil, nil).Schema())
				}
			default:
				messageSchema = NewProtoParser(field.Message(), nil, nil).Schema()
			}

			instance.OptionalField(nameable.NewSimpleNameable(fieldName), messageSchema, func(node *node.Node) error {
				switch {
				case field.IsMap():
					fieldInstance := instance.proto.NewField(field)

					mappingRetriever := func(env map[string]string) (map[string]string, error) {
						return node.GetStringMapping()
					}

					if field.Name() == "docker_arguments" {
						mappingRetriever = node.GetMapOrListOfMapsWithExpansion
					}

					mapping, err := mappingRetriever(mergedEnv)
					if err != nil {
						return err
					}
					var keys []string
					for key := range mapping {
						keys = append(keys, key)
					}
					// determenistic order
					sort.Strings(keys)
					for _, key := range keys {
						fieldInstance.Map().Set(
							protoreflect.ValueOfString(key).MapKey(),
							protoreflect.ValueOfString(mapping[key]),
						)
					}
					instance.proto.Set(field, fieldInstance)
					return nil
				case field.IsList():
					fieldInstance := instance.proto.NewField(field)
					for _, child := range node.Children {
						var err error
						var parsedChild *dynamicpb.Message
						// a little bit of magic to support port forwarding via `port` field instead of two fields
						if fieldName == "additional_containers" {
							childParser := NewAdditionalContainer(mergedEnv, parserKit)
							additionalContainer, err := childParser.Parse(child, parserKit)
							if err != nil {
								return err
							}
							additionalContainerBytes, err := proto.Marshal(additionalContainer)
							if err != nil {
								return err
							}
							parsedChild = dynamicpb.NewMessage(field.Message())
							//nolint:ineffassign,staticcheck
							err = proto.Unmarshal(additionalContainerBytes, parsedChild)
						} else {
							childParser := NewProtoParser(field.Message(), mergedEnv, parserKit)
							parsedChild, err = childParser.Parse(child, parserKit)
						}
						if err != nil {
							return err
						}
						fieldInstance.List().Append(protoreflect.ValueOfMessage(parsedChild))
					}
					instance.proto.Set(field, fieldInstance)
					return nil
				default:
					childParser := NewProtoParser(field.Message(), mergedEnv, parserKit)
					parserChild, err := childParser.Parse(node, parserKit)
					if err != nil {
						return err
					}
					instance.proto.Set(field, protoreflect.ValueOfMessage(parserChild))
					return nil
				}
			})
		case protoreflect.EnumKind:
			var enumItems []interface{}
			for i := 0; i < field.Enum().Values().Len(); i++ {
				name := string(field.Enum().Values().Get(i).Name())
				enumItems = append(enumItems, strings.ToLower(name))
			}
			enumSchema := schema.Enum(enumItems, fieldDescription)

			instance.OptionalField(nameable.NewSimpleNameable(fieldName), enumSchema, func(node *node.Node) error {
				value, err := node.GetExpandedStringValue(mergedEnv)
				if err != nil {
					return err
				}
				enumValueDescriptor := field.Enum().Values().ByName(protoreflect.Name(strings.ToUpper(value)))
				instance.proto.Set(field, protoreflect.ValueOfEnum(enumValueDescriptor.Number()))
				return nil
			})
		case protoreflect.StringKind:
			if field.Cardinality() == protoreflect.Repeated {
				repeatedSchema := schema.ArrayOf(schema.String(fieldDescription))

				instance.OptionalField(nameable.NewSimpleNameable(fieldName), repeatedSchema, func(node *node.Node) error {
					values, err := node.GetSliceOfExpandedStrings(mergedEnv)
					if err != nil {
						return err
					}
					fieldInstance := instance.proto.NewField(field)
					for _, value := range values {
						fieldInstance.List().Append(protoreflect.ValueOfString(value))
					}
					instance.proto.Set(field, fieldInstance)
					return nil
				})
			} else {
				parseCallback := func(node *node.Node) error {
					value, err := node.GetExpandedStringValue(mergedEnv)
					if err != nil {
						return err
					}
					instance.proto.Set(field, protoreflect.ValueOfString(value))
					return nil
				}
				if strings.HasSuffix(fieldName, "credentials") || strings.HasSuffix(fieldName, "config") {
					// some trickery to be able to specify top level credentials for instances
					instance.CollectibleField(fieldName, schema.String(fieldDescription), parseCallback)
				} else {
					instance.OptionalField(nameable.NewSimpleNameable(fieldName), schema.String(fieldDescription), parseCallback)
				}
			}
		case protoreflect.Int64Kind, protoreflect.Sint64Kind,
			protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind:
			intSchema := schema.Integer(fieldDescription)

			instance.OptionalField(nameable.NewSimpleNameable(fieldName), intSchema, func(node *node.Node) error {
				value, err := node.GetExpandedStringValue(mergedEnv)
				if err != nil {
					return err
				}
				var parsedValue int64
				if strings.EqualFold(fieldName, "memory") {
					parsedValue, err = resources.ParseMegaBytes(value)
				} else {
					parsedValue, err = strconv.ParseInt(value, 10, 64)
				}
				if err != nil {
					return node.ParserError("%s", err.Error())
				}
				instance.proto.Set(field, protoreflect.ValueOfInt64(parsedValue))
				return nil
			})
		case protoreflect.Uint64Kind:
			intSchema := schema.Integer(fieldDescription)

			instance.OptionalField(nameable.NewSimpleNameable(fieldName), intSchema, func(node *node.Node) error {
				value, err := node.GetExpandedStringValue(mergedEnv)
				if err != nil {
					return err
				}
				var parsedValue int64
				if strings.EqualFold(fieldName, "memory") {
					parsedValue, err = resources.ParseMegaBytes(value)
				} else {
					parsedValue, err = strconv.ParseInt(value, 10, 64)
				}
				if err != nil {
					return node.ParserError("%s", err.Error())
				}
				instance.proto.Set(field, protoreflect.ValueOfUint64(uint64(parsedValue)))
				return nil
			})
		case protoreflect.Int32Kind, protoreflect.Sint32Kind,
			protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind:
			intSchema := schema.Integer(fieldDescription)

			instance.OptionalField(nameable.NewSimpleNameable(fieldName), intSchema, func(node *node.Node) error {
				value, err := node.GetExpandedStringValue(mergedEnv)
				if err != nil {
					return err
				}
				parsedValue, err := strconv.ParseInt(value, 10, 32)
				if strings.EqualFold(fieldName, "memory") {
					parsedValue, err = resources.ParseMegaBytes(value)
				}
				if err != nil {
					return node.ParserError("%s", err.Error())
				}
				instance.proto.Set(field, protoreflect.ValueOfInt32(int32(parsedValue)))
				return nil
			})
		case protoreflect.Uint32Kind:
			intSchema := schema.Integer(fieldDescription)

			instance.OptionalField(nameable.NewSimpleNameable(fieldName), intSchema, func(node *node.Node) error {
				value, err := node.GetExpandedStringValue(mergedEnv)
				if err != nil {
					return err
				}
				parsedValue, err := strconv.ParseInt(value, 10, 32)
				if strings.EqualFold(fieldName, "memory") {
					parsedValue, err = resources.ParseMegaBytes(value)
				}
				if err != nil {
					return node.ParserError("%s", err.Error())
				}
				instance.proto.Set(field, protoreflect.ValueOfUint32(uint32(parsedValue)))
				return nil
			})
		case protoreflect.BoolKind:
			boolSchema := schema.Boolean(fieldDescription)

			instance.OptionalField(nameable.NewSimpleNameable(fieldName), boolSchema, func(node *node.Node) error {
				evaluation, err := node.GetBoolValue(mergedEnv, parserKit.Boolevator)
				if err != nil {
					return err
				}
				instance.proto.Set(field, protoreflect.ValueOfBool(evaluation))
				return nil
			})
		case protoreflect.FloatKind:
			numberSchema := schema.Number(fieldDescription)

			instance.OptionalField(nameable.NewSimpleNameable(fieldName), numberSchema, func(node *node.Node) error {
				value, err := node.GetExpandedStringValue(mergedEnv)
				if err != nil {
					return err
				}
				parsedValue, err := strconv.ParseFloat(value, 32)
				if err != nil {
					return err
				}
				instance.proto.Set(field, protoreflect.ValueOfFloat32(float32(parsedValue)))
				return nil
			})
		case protoreflect.DoubleKind:
			numberSchema := schema.Number(fieldDescription)

			instance.OptionalField(nameable.NewSimpleNameable(fieldName), numberSchema, func(node *node.Node) error {
				value, err := node.GetExpandedStringValue(mergedEnv)
				if err != nil {
					return err
				}
				parsedValue, err := strconv.ParseFloat(value, 64)
				if err != nil {
					return err
				}
				instance.proto.Set(field, protoreflect.ValueOfFloat64(parsedValue))
				return nil
			})
		case protoreflect.GroupKind, protoreflect.BytesKind:
			// not supported
		}
	}

	return instance
}

func (p *ProtoInstance) Parse(node *node.Node, parserKit *parserkit.ParserKit) (*dynamicpb.Message, error) {
	if err := p.DefaultParser.Parse(node, parserKit); err != nil {
		return nil, err
	}
	return p.proto, nil
}

//nolint:goconst
func GuessPlatform(anyInstance *anypb.Any, descriptor protoreflect.MessageDescriptor) string {
	instanceType := strings.ToLower(anyInstance.TypeUrl)
	if strings.Contains(instanceType, "windows") {
		return "windows"
	}
	if strings.Contains(instanceType, "freebsd") {
		return "freebsd"
	}
	if strings.Contains(instanceType, "darwin") {
		return "darwin"
	}
	if strings.Contains(instanceType, "osx") {
		return "darwin"
	}
	if strings.Contains(instanceType, "anka") {
		return "darwin"
	}
	if strings.Contains(instanceType, "macos") {
		return "darwin"
	}

	dynamicMessage := dynamicpb.NewMessage(descriptor)
	_ = proto.Unmarshal(anyInstance.GetValue(), dynamicMessage)
	return GuessPlatformOfProtoMessage(dynamicMessage, descriptor)
}

func GuessPlatformOfProtoMessage(message protoreflect.Message, descriptor protoreflect.MessageDescriptor) string {
	fields := descriptor.Fields()
	platformField := fields.ByJSONName("platform")
	if platformField != nil && message.Has(platformField) {
		value := message.Get(platformField)
		valueDescription := platformField.Enum().Values().Get(int(value.Enum()))
		enumName := string(valueDescription.Name())
		return strings.ToLower(enumName)
	}
	if platformField != nil {
		// there is platform field but it's not set so let's return the default
		return "linux"
	}
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		// recursively check only message fields but not lists since 'platform' shouldn't be in a repeated field
		if field.Kind() != protoreflect.MessageKind || field.Cardinality() == protoreflect.Repeated {
			continue
		}
		if !message.Has(field) {
			continue
		}
		fieldValue := message.Get(field)
		platformOfField := GuessPlatformOfProtoMessage(fieldValue.Message(), field.Message())
		if platformOfField != "" {
			return platformOfField
		}
	}
	return ""
}

func GuessArchitectureOfProtoMessage(anyInstance *anypb.Any, descriptor protoreflect.MessageDescriptor) string {
	message := dynamicpb.NewMessage(descriptor)
	_ = proto.Unmarshal(anyInstance.GetValue(), message)
	fields := descriptor.Fields()
	architectureField := fields.ByJSONName("architecture")
	if architectureField != nil && message.Has(architectureField) {
		value := message.Get(architectureField)
		valueDescription := architectureField.Enum().Values().Get(int(value.Enum()))
		enumName := string(valueDescription.Name())
		return strings.ToLower(enumName)
	}
	return ""
}

func (p *ProtoInstance) Schema() *jsschema.Schema {
	modifiedSchema := p.DefaultParser.Schema()

	modifiedSchema.Type = jsschema.PrimitiveTypes{jsschema.ObjectType}

	return modifiedSchema
}
