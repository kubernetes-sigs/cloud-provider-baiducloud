/*
Copyright 2018 The Kubernetes Authors.

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

package cloud_provider

import (
	"context"
	"fmt"

	"github.com/golang/glog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/cloudprovider"

	"k8s.io/cloud-provider-baiducloud/pkg/sdk/cce"
	"k8s.io/cloud-provider-baiducloud/pkg/sdk/vpc"
)

// Routes returns a routes interface along with whether the interface is supported.
func (bc *Baiducloud) Routes() (cloudprovider.Routes, bool) {
	return bc, true
}

// ListRoutes lists all managed routes that belong to the specified clusterName
func (bc *Baiducloud) ListRoutes(ctx context.Context, clusterName string) (routes []*cloudprovider.Route, err error) {
	vpcid, err := bc.getVpcID()
	if err != nil {
		return nil, err
	}
	args := vpc.ListRouteArgs{
		VpcID: vpcid,
	}
	rs, err := bc.clientSet.Vpc().ListRouteTable(&args)
	if err != nil {
		return nil, err
	}
	inss, err := bc.clientSet.Cce().ListInstances(bc.ClusterID)
	if err != nil {
		return nil, err
	}
	var kubeRoutes []*cloudprovider.Route
	nodename := make(map[string]string)
	for _, ins := range inss {
		nodename[ins.InstanceId] = ins.InternalIP
	}
	for _, r := range rs {
		// filter instance route
		if r.NexthopType != "custom" {
			continue
		}
		var insName string
		insName, ok := nodename[r.NexthopID]
		if !ok {
			continue
		}
		route := &cloudprovider.Route{
			Name:            r.RouteRuleID,
			DestinationCIDR: r.DestinationAddress,
			TargetNode:      types.NodeName(insName),
		}
		vpcId, err := bc.getVpcID()
		if err != nil {
			return nil, err
		}
		err = bc.ensureRouteInfoToNode(string(route.TargetNode), vpcId, r.RouteTableID, r.RouteRuleID)
		if err != nil {
			return nil, err
		}
		kubeRoutes = append(kubeRoutes, route)
	}
	return kubeRoutes, nil
}

// CreateRoute creates the described managed route
// route.Name will be ignored, although the cloud-provider may use nameHint
// to create a more user-meaningful name.
func (bc *Baiducloud) CreateRoute(ctx context.Context, clusterName string, nameHint string, kubeRoute *cloudprovider.Route) error {
	glog.V(3).Infof("CreateRoute: creating route. clusterName=%v instance=%v cidr=%v", clusterName, kubeRoute.TargetNode, kubeRoute.DestinationCIDR)
	vpcRoutes, err := bc.getVpcRouteTable()
	if err != nil {
		return err
	}
	if len(vpcRoutes) < 1 {
		return fmt.Errorf("VPC route length error: length is : %d", len(vpcRoutes))
	}
	var insID string
	inss, err := bc.clientSet.Cce().ListInstances(bc.ClusterID)
	if err != nil {
		return err
	}
	for _, ins := range inss {
		if ins.InternalIP == string(kubeRoute.TargetNode) {
			insID = ins.InstanceId
			if ins.Status == cce.InstanceStatusCreateFailed || ins.Status == cce.InstanceStatusDeleted || ins.Status == cce.InstanceStatusDeleting || ins.Status == cce.InstanceStatusError {
				glog.V(3).Infof("No need to create route, instance has a wrong status: %s", ins.Status)
				return nil
			}
			break
		}
	}

	// update
	var needDelete []string
	for _, vr := range vpcRoutes {
		if vr.DestinationAddress == kubeRoute.DestinationCIDR && vr.SourceAddress == "0.0.0.0/0" && vr.NexthopID == insID {
			glog.V(3).Infof("Route rule already exists.")
			return nil
		}
		if vr.DestinationAddress == kubeRoute.DestinationCIDR && vr.SourceAddress == "0.0.0.0/0" {
			needDelete = append(needDelete, vr.RouteRuleID)
		}
	}
	if len(needDelete) > 0 {
		for _, delRoute := range needDelete {
			err := bc.clientSet.Vpc().DeleteRoute(delRoute)
			if err != nil {
				glog.V(3).Infof("Delete VPC route error %s", err)
				return err
			}
		}
	}

	args := vpc.CreateRouteRuleArgs{
		RouteTableID:       vpcRoutes[0].RouteTableID,
		NexthopType:        "custom",
		Description:        fmt.Sprintf("auto generated by cce:%s", bc.ClusterID),
		DestinationAddress: kubeRoute.DestinationCIDR,
		SourceAddress:      "0.0.0.0/0",
		NexthopID:          insID,
	}
	glog.V(3).Infof("CreateRoute: create args %v", args)
	routeRuleID, err := bc.clientSet.Vpc().CreateRouteRule(&args)
	if err != nil {
		return err
	}

	vpcId, err := bc.getVpcID()
	if err != nil {
		return err
	}
	err = bc.ensureRouteInfoToNode(string(kubeRoute.TargetNode), vpcId, vpcRoutes[0].RouteTableID, routeRuleID)
	if err != nil {
		return err
	}

	glog.V(3).Infof("CreateRoute for cluster: %v node: %v success", clusterName, kubeRoute.TargetNode)
	return nil
}

// DeleteRoute deletes the specified managed route
// Route should be as returned by ListRoutes
func (bc *Baiducloud) DeleteRoute(ctx context.Context, clusterName string, kubeRoute *cloudprovider.Route) error {
	glog.V(3).Infof("DeleteRoute: deleting route. clusterName=%q instance=%q cidr=%q", clusterName, kubeRoute.TargetNode, kubeRoute.DestinationCIDR)
	vpcTable, err := bc.getVpcRouteTable()
	if err != nil {
		glog.V(3).Infof("getVpcRouteTable error %s", err.Error())
		return err
	}
	for _, vr := range vpcTable {
		if vr.DestinationAddress == kubeRoute.DestinationCIDR && vr.SourceAddress == "0.0.0.0/0" {
			glog.V(3).Infof("DeleteRoute: DestinationAddress is %s .", vr.DestinationAddress)
			err := bc.clientSet.Vpc().DeleteRoute(vr.RouteRuleID)
			if err != nil {
				glog.V(3).Infof("Delete VPC route error %s", err.Error())
				return err
			}
		}
	}

	glog.V(3).Infof("DeleteRoute: success, clusterName=%q instance=%q cidr=%q", clusterName, kubeRoute.TargetNode, kubeRoute.DestinationCIDR)

	return nil
}

func (bc *Baiducloud) getVpcRouteTable() ([]vpc.RouteRule, error) {
	vpcid, err := bc.getVpcID()
	if err != nil {
		return nil, err
	}
	args := vpc.ListRouteArgs{
		VpcID: vpcid,
	}
	rs, err := bc.clientSet.Vpc().ListRouteTable(&args)
	if err != nil {
		return nil, err
	}
	return rs, nil
}

// ensureRouteInfoToNode add below annotation to node
// node.alpha.kubernetes.io/vpc-id: "vpc-xxx"
// node.alpha.kubernetes.io/vpc-route-table-id: "rt-xxx"
// node.alpha.kubernetes.io/vpc-route-rule-id: "rr-xxx"
func (bc *Baiducloud) ensureRouteInfoToNode(nodeName, vpcId, vpcRouteTableId, vpcRouteRuleId string) error {
	curNode, err := bc.kubeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if curNode.Annotations == nil {
		curNode.Annotations = make(map[string]string)
	}
	nodeAnnotation := ExtractNodeAnnotation(curNode)
	if nodeAnnotation.VpcId != vpcId {
		curNode.Annotations[NodeAnnotationVpcId] = vpcId
	}
	if nodeAnnotation.VpcRouteTableId != vpcRouteTableId {
		curNode.Annotations[NodeAnnotationVpcRouteTableId] = vpcRouteTableId
	}
	if nodeAnnotation.VpcRouteRuleId != vpcRouteRuleId {
		curNode.Annotations[NodeAnnotationVpcRouteRuleId] = vpcRouteRuleId
	}
	_, err = bc.kubeClient.CoreV1().Nodes().Update(curNode)
	if err != nil {
		return err
	}
	return nil
}

func (bc *Baiducloud) getVpcID() (string, error) {
	if bc.VpcID == "" {
		ins, err := bc.clientSet.Cce().ListInstances(bc.ClusterID)
		if err != nil {
			return "", err
		}
		if len(ins) > 0 {
			bc.VpcID = ins[0].VpcId
			bc.SubnetID = ins[0].SubnetId
		} else {
			return "", fmt.Errorf("Get vpcid error\n")
		}
	}
	return bc.VpcID, nil
}
