/*
Copyright 2020 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package table

import (
	"context"
	"encoding/json"
	"sort"

	awsgo "github.com/aws/aws-sdk-go/aws"
	svcsdk "github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/pkg/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	svcapitypes "github.com/crossplane/provider-aws/apis/dynamodb/v1alpha1"
	aws "github.com/crossplane/provider-aws/pkg/clients"
)

const (
	errUpdateFailed = "cannot update Table"
)

// SetupTable adds a controller that reconciles Table.
func SetupTable(mgr ctrl.Manager, l logging.Logger) error {
	name := managed.ControllerName(svcapitypes.TableGroupKind)
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&svcapitypes.Table{}).
		Complete(managed.NewReconciler(mgr,
			resource.ManagedKind(svcapitypes.TableGroupVersionKind),
			managed.WithExternalConnecter(&connector{kube: mgr.GetClient()}),
			managed.WithInitializers(
				managed.NewNameAsExternalName(mgr.GetClient()),
				managed.NewDefaultProviderConfig(mgr.GetClient()),
				&tagger{kube: mgr.GetClient()}),
			managed.WithLogger(l.WithValues("controller", name)),
			managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name)))))
}

func (*external) preObserve(context.Context, *svcapitypes.Table) error {
	return nil
}
func (*external) postObserve(_ context.Context, cr *svcapitypes.Table, resp *svcsdk.DescribeTableOutput, obs managed.ExternalObservation, err error) (managed.ExternalObservation, error) {
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	switch aws.StringValue(resp.Table.TableStatus) {
	case string(svcapitypes.TableStatus_SDK_CREATING):
		cr.SetConditions(v1alpha1.Creating())
	case string(svcapitypes.TableStatus_SDK_DELETING):
		cr.SetConditions(v1alpha1.Deleting())
	case string(svcapitypes.TableStatus_SDK_ACTIVE):
		cr.SetConditions(v1alpha1.Available())
	case string(svcapitypes.TableStatus_SDK_ARCHIVED), string(svcapitypes.TableStatus_SDK_INACCESSIBLE_ENCRYPTION_CREDENTIALS), string(svcapitypes.TableStatus_SDK_ARCHIVING):
		cr.SetConditions(v1alpha1.Unavailable())
	}
	return obs, nil
}

func (*external) preCreate(context.Context, *svcapitypes.Table) error {
	return nil
}

func (*external) postCreate(_ context.Context, _ *svcapitypes.Table, _ *svcsdk.CreateTableOutput, cre managed.ExternalCreation, err error) (managed.ExternalCreation, error) {
	return cre, err
}

func (*external) postUpdate(_ context.Context, _ *svcapitypes.Table, upd managed.ExternalUpdate, err error) (managed.ExternalUpdate, error) {
	return upd, err
}

func preGenerateDescribeTableInput(_ *svcapitypes.Table, obj *svcsdk.DescribeTableInput) *svcsdk.DescribeTableInput {
	return obj
}

func postGenerateDescribeTableInput(cr *svcapitypes.Table, obj *svcsdk.DescribeTableInput) *svcsdk.DescribeTableInput {
	obj.TableName = aws.String(meta.GetExternalName(cr))
	return obj
}

func preGenerateCreateTableInput(_ *svcapitypes.Table, obj *svcsdk.CreateTableInput) *svcsdk.CreateTableInput {
	return obj
}

func postGenerateCreateTableInput(cr *svcapitypes.Table, obj *svcsdk.CreateTableInput) *svcsdk.CreateTableInput {
	obj.TableName = aws.String(meta.GetExternalName(cr))
	return obj
}
func preGenerateDeleteTableInput(_ *svcapitypes.Table, obj *svcsdk.DeleteTableInput) *svcsdk.DeleteTableInput {
	return obj
}

func postGenerateDeleteTableInput(cr *svcapitypes.Table, obj *svcsdk.DeleteTableInput) *svcsdk.DeleteTableInput {
	obj.TableName = aws.String(meta.GetExternalName(cr))
	return obj
}

type tagger struct {
	kube client.Client
}

func (e *tagger) Initialize(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*svcapitypes.Table)
	if !ok {
		return errors.New(errUnexpectedObject)
	}
	tagMap := map[string]string{}
	for _, t := range cr.Spec.ForProvider.Tags {
		tagMap[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
	}
	for k, v := range resource.GetExternalTags(cr) {
		tagMap[k] = v
	}
	tags := make([]*svcapitypes.Tag, 0)
	for k, v := range tagMap {
		tags = append(tags, &svcapitypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	sort.Slice(tags, func(i, j int) bool {
		return aws.StringValue(tags[i].Key) < aws.StringValue(tags[j].Key)
	})
	if cmp.Equal(cr.Spec.ForProvider.Tags, tags) {
		return nil
	}
	cr.Spec.ForProvider.Tags = tags
	return errors.Wrap(e.kube.Update(ctx, cr), "cannot update Table Spec")
}

// NOTE(muvaf): The rest is taken from manually written controller.

func lateInitialize(in *svcapitypes.TableParameters, t *svcsdk.DescribeTableOutput) error { // nolint:gocyclo,unparam
	if t == nil {
		return nil
	}

	if len(in.AttributeDefinitions) == 0 && len(t.Table.AttributeDefinitions) != 0 {
		in.AttributeDefinitions = buildAttributeDefinitions(t.Table.AttributeDefinitions)
	}

	if len(in.GlobalSecondaryIndexes) == 0 && len(t.Table.GlobalSecondaryIndexes) != 0 {
		in.GlobalSecondaryIndexes = buildGlobalIndexes(t.Table.GlobalSecondaryIndexes)
	}

	if len(in.LocalSecondaryIndexes) == 0 && len(t.Table.LocalSecondaryIndexes) != 0 {
		in.LocalSecondaryIndexes = buildLocalIndexes(t.Table.LocalSecondaryIndexes)
	}

	if len(in.KeySchema) == 0 && len(t.Table.KeySchema) != 0 {
		in.KeySchema = buildAlphaKeyElements(t.Table.KeySchema)
	}

	if in.ProvisionedThroughput == nil && t.Table.ProvisionedThroughput != nil {
		in.ProvisionedThroughput = &svcapitypes.ProvisionedThroughput{
			ReadCapacityUnits:  t.Table.ProvisionedThroughput.ReadCapacityUnits,
			WriteCapacityUnits: t.Table.ProvisionedThroughput.WriteCapacityUnits,
		}
	}
	if in.SSESpecification == nil && t.Table.SSEDescription != nil {
		in.SSESpecification = &svcapitypes.SSESpecification{
			SSEType: t.Table.SSEDescription.SSEType,
		}
	}
	if in.StreamSpecification == nil && t.Table.StreamSpecification != nil {
		in.StreamSpecification = &svcapitypes.StreamSpecification{
			StreamEnabled:  t.Table.StreamSpecification.StreamEnabled,
			StreamViewType: t.Table.StreamSpecification.StreamViewType,
		}
	}
	return nil
}

func buildAlphaKeyElements(keys []*svcsdk.KeySchemaElement) []*svcapitypes.KeySchemaElement {
	if len(keys) == 0 {
		return nil
	}
	keyElements := make([]*svcapitypes.KeySchemaElement, len(keys))
	for i, val := range keys {
		keyElements[i] = &svcapitypes.KeySchemaElement{
			AttributeName: val.AttributeName,
			KeyType:       val.KeyType,
		}
	}
	return keyElements
}

func buildAttributeDefinitions(attributes []*svcsdk.AttributeDefinition) []*svcapitypes.AttributeDefinition {
	if len(attributes) == 0 {
		return nil
	}
	attributeDefinitions := make([]*svcapitypes.AttributeDefinition, len(attributes))
	for i, val := range attributes {
		attributeDefinitions[i] = &svcapitypes.AttributeDefinition{
			AttributeName: val.AttributeName,
			AttributeType: val.AttributeType,
		}
	}
	return attributeDefinitions
}

func buildGlobalIndexes(indexes []*svcsdk.GlobalSecondaryIndexDescription) []*svcapitypes.GlobalSecondaryIndex {
	if len(indexes) == 0 {
		return nil
	}
	globalSecondaryIndexes := make([]*svcapitypes.GlobalSecondaryIndex, len(indexes))
	for i, val := range indexes {
		globalSecondaryIndexes[i] = &svcapitypes.GlobalSecondaryIndex{
			IndexName: val.IndexName,
			KeySchema: buildAlphaKeyElements(val.KeySchema),
		}
		if val.Projection != nil {
			globalSecondaryIndexes[i].Projection = &svcapitypes.Projection{
				NonKeyAttributes: val.Projection.NonKeyAttributes,
				ProjectionType:   val.Projection.ProjectionType,
			}
		}
	}
	return globalSecondaryIndexes
}

func buildLocalIndexes(indexes []*svcsdk.LocalSecondaryIndexDescription) []*svcapitypes.LocalSecondaryIndex {
	if len(indexes) == 0 {
		return nil
	}
	localSecondaryIndexes := make([]*svcapitypes.LocalSecondaryIndex, len(indexes))
	for i, val := range indexes {
		localSecondaryIndexes[i] = &svcapitypes.LocalSecondaryIndex{
			IndexName: val.IndexName,
			KeySchema: buildAlphaKeyElements(val.KeySchema),
		}
		if val.Projection != nil {
			localSecondaryIndexes[i].Projection = &svcapitypes.Projection{
				NonKeyAttributes: val.Projection.NonKeyAttributes,
				ProjectionType:   val.Projection.ProjectionType,
			}
		}
	}
	return localSecondaryIndexes
}

// CreatePatch creates a *svcapitypes.TableParameters that has only the changed
// values between the target *svcapitypes.TableParameters and the current
// *dynamodb.TableDescription
func CreatePatch(in *svcsdk.DescribeTableOutput, target *svcapitypes.TableParameters) (*svcapitypes.TableParameters, error) {
	currentParams := &svcapitypes.TableParameters{}
	if err := lateInitialize(currentParams, in); err != nil {
		return nil, err
	}

	jsonPatch, err := aws.CreateJSONPatch(currentParams, target)
	if err != nil {
		return nil, err
	}
	patch := &svcapitypes.TableParameters{}
	if err := json.Unmarshal(jsonPatch, patch); err != nil {
		return nil, err
	}
	return patch, nil
}

func isUpToDate(cr *svcapitypes.Table, resp *svcsdk.DescribeTableOutput) bool {
	patch, err := CreatePatch(resp, &cr.Spec.ForProvider)
	if err != nil {
		return false
	}
	return cmp.Equal(&svcapitypes.TableParameters{}, patch,
		cmpopts.IgnoreTypes(&v1alpha1.Reference{}, &v1alpha1.Selector{}, []v1alpha1.Reference{}),
		cmpopts.IgnoreFields(svcapitypes.TableParameters{}, "Region", "Tags", "GlobalSecondaryIndexes", "KeySchema", "LocalSecondaryIndexes", "CustomTableParameters"))
}

func (e *external) preUpdate(ctx context.Context, cr *svcapitypes.Table) error {
	switch aws.StringValue(cr.Status.AtProvider.TableStatus) {
	case string(svcapitypes.TableStatus_SDK_UPDATING), string(svcapitypes.TableStatus_SDK_CREATING):
		return nil
	}
	t, err := e.client.DescribeTable(&svcsdk.DescribeTableInput{TableName: aws.String(meta.GetExternalName(cr))})
	if err != nil {
		return errors.Wrap(err, errDescribe)
	}

	u := GenerateUpdateTableInput(meta.GetExternalName(cr), &cr.Spec.ForProvider)
	// NOTE(muvaf): AWS API prohibits doing those calls in the same call.
	// See https://github.com/aws/aws-sdk-go/blob/v1.34.32/service/dynamodb/api.go#L5605
	switch {
	case aws.Int64Value(t.Table.ProvisionedThroughput.ReadCapacityUnits) != aws.Int64Value(cr.Spec.ForProvider.ProvisionedThroughput.ReadCapacityUnits) ||
		aws.Int64Value(t.Table.ProvisionedThroughput.WriteCapacityUnits) != aws.Int64Value(cr.Spec.ForProvider.ProvisionedThroughput.WriteCapacityUnits):
		u.ProvisionedThroughput = &svcsdk.ProvisionedThroughput{
			WriteCapacityUnits: cr.Spec.ForProvider.ProvisionedThroughput.WriteCapacityUnits,
			ReadCapacityUnits:  cr.Spec.ForProvider.ProvisionedThroughput.ReadCapacityUnits,
		}
	case awsgo.BoolValue(t.Table.StreamSpecification.StreamEnabled) != awsgo.BoolValue(cr.Spec.ForProvider.StreamSpecification.StreamEnabled):
		u.StreamSpecification = &svcsdk.StreamSpecification{StreamEnabled: cr.Spec.ForProvider.StreamSpecification.StreamEnabled}
	}
	// TODO(muvaf): ReplicationGroupUpdate and GlobalSecondaryIndexUpdate features
	// are not implemented yet.

	_, err = e.client.UpdateTableWithContext(ctx, u)
	return errors.Wrap(err, errUpdateFailed)
}

// GenerateUpdateTableInput from TableParameters.
func GenerateUpdateTableInput(name string, p *svcapitypes.TableParameters) *svcsdk.UpdateTableInput {
	u := &svcsdk.UpdateTableInput{
		TableName: aws.String(name),
	}

	if len(p.AttributeDefinitions) != 0 {
		u.AttributeDefinitions = make([]*svcsdk.AttributeDefinition, len(p.AttributeDefinitions))
		for i, val := range p.AttributeDefinitions {
			u.AttributeDefinitions[i] = &svcsdk.AttributeDefinition{
				AttributeName: val.AttributeName,
				AttributeType: val.AttributeType,
			}
		}
	}

	if p.SSESpecification != nil {
		u.SSESpecification = &svcsdk.SSESpecification{
			Enabled:        p.SSESpecification.Enabled,
			KMSMasterKeyId: p.SSESpecification.KMSMasterKeyID,
			SSEType:        p.SSESpecification.SSEType,
		}
	}

	return u
}
