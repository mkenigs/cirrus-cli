package task

import (
	"github.com/cirruslabs/cirrus-ci-agent/api"
	"github.com/cirruslabs/cirrus-cli/internal/executor/environment"
	"github.com/cirruslabs/cirrus-cli/pkg/parser/boolevator"
	"github.com/cirruslabs/cirrus-cli/pkg/parser/nameable"
	"github.com/cirruslabs/cirrus-cli/pkg/parser/node"
	"github.com/cirruslabs/cirrus-cli/pkg/parser/parseable"
	"github.com/cirruslabs/cirrus-cli/pkg/parser/parserkit"
	"github.com/cirruslabs/cirrus-cli/pkg/parser/schema"
	"github.com/cirruslabs/cirrus-cli/pkg/parser/task/command"
	"github.com/golang/protobuf/protoc-gen-go/descriptor"
	jsschema "github.com/lestrrat-go/jsschema"
	"google.golang.org/protobuf/types/known/anypb"
	"strconv"
	"strings"
)

type DockerBuilder struct {
	proto api.Task

	platform  api.Platform
	osVersion string

	fallbackName string
	alias        string
	dependsOn    []string

	onlyIfExpression string

	parseable.DefaultParser
}

func NewDockerBuilder(
	env map[string]string,
	parserKit *parserkit.ParserKit,
	additionalTaskProperties []*descriptor.FieldDescriptorProto,
) *DockerBuilder {
	dbuilder := &DockerBuilder{}
	dbuilder.proto.Environment = map[string]string{"CIRRUS_OS": "linux"}

	AttachEnvironmentFields(&dbuilder.DefaultParser, &dbuilder.proto)
	AttachBaseTaskFields(&dbuilder.DefaultParser, &dbuilder.proto, env, parserKit, additionalTaskProperties)
	AttachBaseTaskInstructions(&dbuilder.DefaultParser, &dbuilder.proto, env, parserKit)

	dbuilder.OptionalField(nameable.NewSimpleNameable("alias"), schema.String(""), func(node *node.Node) error {
		name, err := node.GetExpandedStringValue(environment.Merge(dbuilder.proto.Environment, env))
		if err != nil {
			return err
		}

		dbuilder.alias = name
		dbuilder.proto.Metadata.Properties["alias"] = name

		return nil
	})

	dependsOnSchema := schema.StringOrListOfStrings("List of task names this task depends on.")
	dbuilder.OptionalField(nameable.NewSimpleNameable("depends_on"), dependsOnSchema, func(node *node.Node) error {
		dependsOn, err := node.GetSliceOfExpandedStrings(environment.Merge(dbuilder.proto.Environment, env))
		if err != nil {
			return err
		}
		dbuilder.dependsOn = dependsOn
		return nil
	})

	dbuilder.CollectibleField("only_if", schema.Condition(""), func(node *node.Node) error {
		onlyIfExpression, err := node.GetStringValue()
		if err != nil {
			return err
		}
		dbuilder.onlyIfExpression = onlyIfExpression
		return nil
	})

	// no-op
	sipSchema := schema.Condition("")
	dbuilder.OptionalField(nameable.NewSimpleNameable("use_static_ip"), sipSchema, func(node *node.Node) error {
		return nil
	})

	platformSchema := schema.Enum([]interface{}{"linux", "windows"}, "Container Platform.")
	dbuilder.OptionalField(nameable.NewSimpleNameable("platform"), platformSchema, func(node *node.Node) error {
		platformName, err := node.GetExpandedStringValue(environment.Merge(dbuilder.proto.Environment, env))
		if err != nil {
			return err
		}

		platformValue, ok := api.Platform_value[strings.ToUpper(platformName)]
		if !ok {
			return node.ParserError("unknown platform name: %q", platformName)
		}

		dbuilder.platform = api.Platform(platformValue)
		dbuilder.proto.Environment["CIRRUS_OS"] = platformName

		return nil
	})

	osVersionSchema := schema.Enum([]interface{}{"2019", "1709", "1803"}, "Windows version of container.")
	dbuilder.OptionalField(nameable.NewSimpleNameable("os_version"), osVersionSchema, func(node *node.Node) error {
		osVersion, err := node.GetExpandedStringValue(environment.Merge(dbuilder.proto.Environment, env))
		if err != nil {
			return err
		}

		dbuilder.osVersion = osVersion

		return nil
	})

	return dbuilder
}

func (dbuilder *DockerBuilder) Parse(node *node.Node, parserKit *parserkit.ParserKit) error {
	if err := dbuilder.DefaultParser.Parse(node, parserKit); err != nil {
		return err
	}

	instance := &api.DockerBuilder{
		Platform:  dbuilder.platform,
		OsVersion: dbuilder.osVersion,
	}

	anyInstance, err := anypb.New(instance)
	if err != nil {
		return err
	}

	dbuilder.proto.Instance = anyInstance

	// Since the parsing is almost done and no other commands are expected,
	// we can safely append cache upload commands, if applicable
	dbuilder.proto.Commands = append(dbuilder.proto.Commands, command.GenUploadCacheCmds(dbuilder.proto.Commands)...)

	return nil
}

func (dbuilder *DockerBuilder) Name() string {
	return dbuilder.proto.Name
}

func (dbuilder *DockerBuilder) SetName(name string) {
	dbuilder.proto.Name = name
}

func (dbuilder *DockerBuilder) FallbackName() string {
	return dbuilder.fallbackName
}

func (dbuilder *DockerBuilder) SetFallbackName(name string) {
	dbuilder.fallbackName = name
}

func (dbuilder *DockerBuilder) Alias() string {
	return dbuilder.alias
}

func (dbuilder *DockerBuilder) DependsOnNames() []string {
	return dbuilder.dependsOn
}

func (dbuilder *DockerBuilder) ID() int64 { return dbuilder.proto.LocalGroupId }
func (dbuilder *DockerBuilder) SetID(id int64) {
	dbuilder.proto.LocalGroupId = id
}

func (dbuilder *DockerBuilder) SetIndexWithinBuild(id int64) {
	dbuilder.proto.Metadata.Properties["indexWithinBuild"] = strconv.FormatInt(id, 10)
}

func (dbuilder *DockerBuilder) Proto() interface{} {
	return &dbuilder.proto
}

func (dbuilder *DockerBuilder) DependsOnIDs() []int64       { return dbuilder.proto.RequiredGroups }
func (dbuilder *DockerBuilder) SetDependsOnIDs(ids []int64) { dbuilder.proto.RequiredGroups = ids }

func (dbuilder *DockerBuilder) Enabled(env map[string]string, boolevator *boolevator.Boolevator) (bool, error) {
	if dbuilder.onlyIfExpression == "" {
		return true, nil
	}

	evaluation, err := boolevator.Eval(dbuilder.onlyIfExpression, environment.Merge(dbuilder.proto.Environment, env))
	if err != nil {
		return false, err
	}

	return evaluation, nil
}

func (dbuilder *DockerBuilder) Schema() *jsschema.Schema {
	modifiedSchema := dbuilder.DefaultParser.Schema()

	modifiedSchema.Type = jsschema.PrimitiveTypes{jsschema.ObjectType}

	return modifiedSchema
}
