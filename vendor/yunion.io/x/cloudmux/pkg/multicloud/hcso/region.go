// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hcso

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud"
	"yunion.io/x/cloudmux/pkg/multicloud/hcso/client"
	"yunion.io/x/cloudmux/pkg/multicloud/huawei/obs"
)

type Locales struct {
	EnUs string `json:"en-us"`
	ZhCN string `json:"zh-cn"`
}

// https://support.huaweicloud.com/api-iam/zh-cn_topic_0067148043.html
type SRegion struct {
	multicloud.SRegion

	client    *SHuaweiClient
	ecsClient *client.Client
	obsClient *obs.ObsClient // 对象存储client.请勿直接引用。

	Description    string  `json:"description"`
	ID             string  `json:"id"`
	Locales        Locales `json:"locales"`
	ParentRegionID string  `json:"parent_region_id"`
	Type           string  `json:"type"`

	izones []cloudprovider.ICloudZone
	ivpcs  []cloudprovider.ICloudVpc
	iskus  []cloudprovider.ICloudSku

	storageCache *SStoragecache
}

func (self *SRegion) list(service, resource string, query url.Values) (jsonutils.JSONObject, error) {
	return self.client.list(service, self.ID, resource, query)
}

func (self *SRegion) delete(service, resource string) (jsonutils.JSONObject, error) {
	return self.client.delete(service, self.ID, resource)
}

func (self *SRegion) put(service, resource string, params map[string]interface{}) (jsonutils.JSONObject, error) {
	return self.client.put(service, self.ID, resource, params)
}

func (self *SRegion) post(service, resource string, params map[string]interface{}) (jsonutils.JSONObject, error) {
	return self.client.post(service, self.ID, resource, params)
}

func (self *SRegion) GetClient() *SHuaweiClient {
	return self.client
}

func (self *SRegion) getECSClient() (*client.Client, error) {
	var err error

	if len(self.client.projectId) > 0 {
		project, err := self.client.GetProjectById(self.client.projectId)
		if err != nil {
			return nil, err
		}

		regionId := strings.Split(project.Name, "_")[0]
		if regionId != self.ID {
			// log.Debugf("project %s not in region %s", self.client.ProjectId, self.ID)
			return nil, errors.Error("region and project mismatch")
		}
	}

	if self.ecsClient == nil {
		self.ecsClient, err = self.client.newRegionAPIClient(self.ID)
		if err != nil {
			return nil, err
		}
	}

	return self.ecsClient, err
}

func (self *SRegion) getOBSEndpoint() string {
	return getOBSEndpoint(self.GetId())
}

func (self *SRegion) getOBSClient() (*obs.ObsClient, error) {
	if self.obsClient == nil {
		obsClient, err := self.client.getOBSClient(self.GetId())
		if err != nil {
			return nil, err
		}

		client := obsClient.GetClient()
		ts, _ := client.Transport.(*http.Transport)
		client.Transport = cloudprovider.GetCheckTransport(ts, func(req *http.Request) (func(resp *http.Response) error, error) {
			if self.client.cpcfg.ReadOnly {
				if req.Method == "GET" || req.Method == "HEAD" {
					return nil, nil
				}
				return nil, errors.Wrapf(cloudprovider.ErrAccountReadOnly, "%s %s", req.Method, req.URL.Path)
			}
			return nil, nil
		})

		self.obsClient = obsClient
	}

	return self.obsClient, nil
}

func (self *SRegion) fetchZones() error {
	zones := make([]SZone, 0)
	err := doListAll(self.ecsClient.Zones.List, nil, &zones)
	if err != nil {
		return err
	}

	self.izones = make([]cloudprovider.ICloudZone, 0)
	for i := range zones {
		zone := zones[i]
		zone.region = self
		self.izones = append(self.izones, &zone)
	}
	return nil
}

func (self *SRegion) fetchIVpcs() error {
	// https://support.huaweicloud.com/api-vpc/zh-cn_topic_0020090625.html
	vpcs := make([]SVpc, 0)
	querys := map[string]string{
		"limit": "2048",
	}
	err := doListAllWithMarker(self.ecsClient.Vpcs.List, querys, &vpcs)
	if err != nil {
		return err
	}

	self.ivpcs = make([]cloudprovider.ICloudVpc, 0)
	for i := range vpcs {
		vpc := vpcs[i]
		vpc.region = self
		self.ivpcs = append(self.ivpcs, &vpc)
	}
	return nil
}

func (self *SRegion) GetIVMById(id string) (cloudprovider.ICloudVM, error) {
	if len(id) == 0 {
		return nil, errors.Wrap(cloudprovider.ErrNotFound, "SRegion.GetIVMById")
	}

	instance, err := self.GetInstanceByID(id)
	if err != nil {
		return nil, err
	}

	zone, err := self.getZoneById(instance.OSEXTAZAvailabilityZone)
	if err != nil {
		return nil, errors.Wrap(err, "getZoneById")
	}
	instance.host = &SHost{
		zone:      zone,
		vms:       nil,
		projectId: self.client.projectId,
		Id:        instance.HostID,
		Name:      instance.OSEXTSRVATTRHost,
	}
	return &instance, err
}

func (self *SRegion) GetIDiskById(id string) (cloudprovider.ICloudDisk, error) {
	return self.GetDisk(id)
}

func (self *SRegion) GetGeographicInfo() cloudprovider.SGeographicInfo {
	if info, ok := LatitudeAndLongitude[self.ID]; ok {
		return info
	}
	return cloudprovider.SGeographicInfo{}
}

func (self *SRegion) GetILoadBalancers() ([]cloudprovider.ICloudLoadbalancer, error) {
	elbs, err := self.GetLoadBalancers()
	if err != nil {
		return nil, err
	}

	ielbs := make([]cloudprovider.ICloudLoadbalancer, len(elbs))
	for i := range elbs {
		ielbs[i] = &elbs[i]
	}

	return ielbs, nil
}

// https://support.huaweicloud.com/api-elb/zh-cn_topic_0096561531.html
func (self *SRegion) GetLoadBalancers() ([]SLoadbalancer, error) {
	params := map[string]string{}
	if len(self.client.projectId) > 0 {
		params["project_id"] = self.client.projectId
	}

	ret := []SLoadbalancer{}
	err := doListAll(self.ecsClient.Elb.List, params, &ret)
	if err != nil {
		return nil, err
	}

	for i := range ret {
		ret[i].region = self
	}

	return ret, nil
}

func (self *SRegion) GetILoadBalancerById(loadbalancerId string) (cloudprovider.ICloudLoadbalancer, error) {
	elb, err := self.GetLoadBalancerById(loadbalancerId)
	if err != nil {
		return nil, err
	}

	return &elb, nil
}

func (self *SRegion) GetLoadBalancerById(loadbalancerId string) (SLoadbalancer, error) {
	elb := SLoadbalancer{}
	err := DoGet(self.ecsClient.Elb.Get, loadbalancerId, nil, &elb)
	if err != nil {
		return elb, err
	}

	elb.region = self
	return elb, nil
}

func (self *SRegion) GetILoadBalancerAclById(aclId string) (cloudprovider.ICloudLoadbalancerAcl, error) {
	acl, err := self.GetLoadBalancerAclById(aclId)
	if err != nil {
		return nil, err
	}

	return &acl, nil
}

func (self *SRegion) GetLoadBalancerAclById(aclId string) (SElbACL, error) {
	acl := SElbACL{}
	err := DoGet(self.ecsClient.ElbWhitelist.Get, aclId, nil, &acl)
	if err != nil {
		return acl, err
	}

	acl.region = self
	return acl, nil
}

func (self *SRegion) GetILoadBalancerCertificateById(certId string) (cloudprovider.ICloudLoadbalancerCertificate, error) {
	cert, err := self.GetLoadBalancerCertificateById(certId)
	if err != nil {
		return nil, err
	}

	return &cert, nil
}

func (self *SRegion) GetLoadBalancerCertificateById(certId string) (SElbCert, error) {
	ret := SElbCert{}
	err := DoGet(self.ecsClient.ElbCertificates.Get, certId, nil, &ret)
	if err != nil {
		return ret, err
	}

	ret.region = self
	return ret, nil
}

func (self *SRegion) CreateILoadBalancerCertificate(cert *cloudprovider.SLoadbalancerCertificate) (cloudprovider.ICloudLoadbalancerCertificate, error) {
	ret, err := self.CreateLoadBalancerCertificate(cert)
	if err != nil {
		return nil, err
	}

	return &ret, nil
}

// https://support.huaweicloud.com/api-elb/zh-cn_topic_0096561584.html
func (self *SRegion) CreateLoadBalancerCertificate(cert *cloudprovider.SLoadbalancerCertificate) (SElbCert, error) {
	params := jsonutils.NewDict()
	params.Set("name", jsonutils.NewString(cert.Name))
	params.Set("private_key", jsonutils.NewString(cert.PrivateKey))
	params.Set("certificate", jsonutils.NewString(cert.Certificate))

	ret := SElbCert{}
	err := DoCreate(self.ecsClient.ElbCertificates.Create, params, &ret)
	if err != nil {
		return ret, err
	}

	ret.region = self
	return ret, nil
}

func (self *SRegion) GetILoadBalancerAcls() ([]cloudprovider.ICloudLoadbalancerAcl, error) {
	ret, err := self.GetLoadBalancerAcls("")
	if err != nil {
		return nil, err
	}

	iret := make([]cloudprovider.ICloudLoadbalancerAcl, len(ret))
	for i := range ret {
		iret[i] = &ret[i]
	}
	return iret, nil
}

// https://support.huaweicloud.com/api-elb/zh-cn_topic_0096561582.html
func (self *SRegion) GetLoadBalancerAcls(listenerId string) ([]SElbACL, error) {
	params := map[string]string{}
	if len(listenerId) > 0 {
		params["listener_id"] = listenerId
	}

	ret := []SElbACL{}
	err := doListAll(self.ecsClient.ElbWhitelist.List, params, &ret)
	if err != nil {
		return nil, err
	}

	for i := range ret {
		ret[i].region = self
	}

	return ret, nil
}

func (self *SRegion) GetILoadBalancerCertificates() ([]cloudprovider.ICloudLoadbalancerCertificate, error) {
	ret, err := self.GetLoadBalancerCertificates()
	if err != nil {
		return nil, err
	}

	iret := make([]cloudprovider.ICloudLoadbalancerCertificate, len(ret))
	for i := range ret {
		iret[i] = &ret[i]
	}
	return iret, nil
}

func (self *SRegion) GetLoadBalancerCertificates() ([]SElbCert, error) {
	ret := []SElbCert{}
	err := doListAll(self.ecsClient.ElbCertificates.List, nil, &ret)
	if err != nil {
		return nil, err
	}

	for i := range ret {
		ret[i].region = self
	}

	return ret, nil
}

// https://support.huaweicloud.com/api-iam/zh-cn_topic_0057845622.html
func (self *SRegion) GetId() string {
	return self.ID
}

func (self *SRegion) GetName() string {
	return fmt.Sprintf("%s %s", CLOUD_PROVIDER_HUAWEI_CN, self.Locales.ZhCN)
}

func (self *SRegion) GetI18n() cloudprovider.SModelI18nTable {
	en := fmt.Sprintf("%s %s", CLOUD_PROVIDER_HUAWEI_EN, self.Locales.EnUs)
	table := cloudprovider.SModelI18nTable{}
	table["name"] = cloudprovider.NewSModelI18nEntry(self.GetName()).CN(self.GetName()).EN(en)
	return table
}

func (self *SRegion) GetGlobalId() string {
	return fmt.Sprintf("%s/%s", api.CLOUD_PROVIDER_HCSO, self.ID)
}

func (self *SRegion) GetStatus() string {
	return api.CLOUD_REGION_STATUS_INSERVER
}

func (self *SRegion) Refresh() error {
	return nil
}

func (self *SRegion) IsEmulated() bool {
	return false
}

func (self *SRegion) GetLatitude() float32 {
	if locationInfo, ok := LatitudeAndLongitude[self.ID]; ok {
		return locationInfo.Latitude
	}
	return 0.0
}

func (self *SRegion) GetLongitude() float32 {
	if locationInfo, ok := LatitudeAndLongitude[self.ID]; ok {
		return locationInfo.Longitude
	}
	return 0.0
}

func (self *SRegion) fetchInfrastructure() error {
	_, err := self.getECSClient()
	if err != nil {
		return err
	}

	if err := self.fetchZones(); err != nil {
		return err
	}

	if err := self.fetchIVpcs(); err != nil {
		return err
	}

	for i := 0; i < len(self.ivpcs); i += 1 {
		vpc := self.ivpcs[i].(*SVpc)
		wire := SWire{region: self, vpc: vpc}
		vpc.addWire(&wire)

		for j := 0; j < len(self.izones); j += 1 {
			zone := self.izones[j].(*SZone)
			zone.addWire(&wire)
		}
	}
	return nil
}

func (self *SRegion) GetIZones() ([]cloudprovider.ICloudZone, error) {
	if self.izones == nil {
		var err error
		err = self.fetchInfrastructure()
		if err != nil {
			return nil, err
		}
	}
	return self.izones, nil
}

func (self *SRegion) GetIVpcs() ([]cloudprovider.ICloudVpc, error) {
	if self.ivpcs == nil {
		err := self.fetchInfrastructure()
		if err != nil {
			return nil, err
		}
	}
	return self.ivpcs, nil
}

func (self *SRegion) GetEipById(eipId string) (SEipAddress, error) {
	var eip SEipAddress
	err := DoGet(self.ecsClient.Eips.Get, eipId, nil, &eip)
	eip.region = self
	return eip, err
}

// 返回参数分别为eip 列表、列表长度、error。
// https://support.huaweicloud.com/api-vpc/zh-cn_topic_0020090598.html
func (self *SRegion) GetEips() ([]SEipAddress, error) {
	querys := make(map[string]string)

	eips := make([]SEipAddress, 0)
	err := doListAllWithMarker(self.ecsClient.Eips.List, querys, &eips)
	for i := range eips {
		eips[i].region = self
	}
	return eips, err
}

func (self *SRegion) GetIEips() ([]cloudprovider.ICloudEIP, error) {
	_, err := self.getECSClient()
	if err != nil {
		return nil, err
	}

	eips, err := self.GetEips()
	if err != nil {
		return nil, err
	}

	ret := make([]cloudprovider.ICloudEIP, len(eips))
	for i := 0; i < len(eips); i += 1 {
		eips[i].region = self
		ret[i] = &eips[i]
	}
	return ret, nil
}

func (self *SRegion) GetIVpcById(id string) (cloudprovider.ICloudVpc, error) {
	ivpcs, err := self.GetIVpcs()
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(ivpcs); i += 1 {
		if ivpcs[i].GetGlobalId() == id {
			return ivpcs[i], nil
		}
	}
	return nil, cloudprovider.ErrNotFound
}

func (self *SRegion) GetIZoneById(id string) (cloudprovider.ICloudZone, error) {
	izones, err := self.GetIZones()
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(izones); i += 1 {
		if izones[i].GetGlobalId() == id {
			return izones[i], nil
		}
	}
	return nil, cloudprovider.ErrNotFound
}

func (self *SRegion) GetIEipById(eipId string) (cloudprovider.ICloudEIP, error) {
	eip, err := self.GetEipById(eipId)
	return &eip, err
}

// https://support.huaweicloud.com/api-vpc/zh-cn_topic_0060595555.html
func (self *SRegion) DeleteSecurityGroup(secgroupId string) error {
	return DoDelete(self.ecsClient.SecurityGroups.Delete, secgroupId, nil, nil)
}

func (self *SRegion) GetISecurityGroupById(secgroupId string) (cloudprovider.ICloudSecurityGroup, error) {
	secgroup, err := self.GetSecurityGroup(secgroupId)
	if err != nil {
		return nil, err
	}
	return secgroup, nil
}

func (self *SRegion) CreateISecurityGroup(opts *cloudprovider.SecurityGroupCreateInput) (cloudprovider.ICloudSecurityGroup, error) {
	return self.CreateSecurityGroup(opts)
}

func (self *SRegion) CreateSecurityGroup(opts *cloudprovider.SecurityGroupCreateInput) (*SSecurityGroup, error) {
	params := map[string]interface{}{
		"name":                  opts.Name,
		"description":           opts.Desc,
		"enterprise_project_id": opts.ProjectId,
	}
	resp, err := self.post(SERVICE_VPC, "vpc/security-groups", map[string]interface{}{"security_group": params})
	if err != nil {
		return nil, err
	}
	ret := &SSecurityGroup{region: self}
	return ret, resp.Unmarshal(ret, "security_group")
}

// https://support.huaweicloud.com/api-vpc/zh-cn_topic_0020090608.html
func (self *SRegion) CreateIVpc(opts *cloudprovider.VpcCreateOptions) (cloudprovider.ICloudVpc, error) {
	return self.CreateVpc(opts.NAME, opts.CIDR, opts.Desc)
}

func (self *SRegion) CreateVpc(name, cidr, desc string) (*SVpc, error) {
	params := map[string]interface{}{
		"vpc": map[string]string{
			"name":        name,
			"cidr":        cidr,
			"description": desc,
		},
	}
	vpc := &SVpc{region: self}
	return vpc, DoCreate(self.ecsClient.Vpcs.Create, jsonutils.Marshal(params), vpc)
}

// https://support.huaweicloud.com/api-vpc/zh-cn_topic_0020090596.html
// size: 1Mbit/s~2000Mbit/s
// bgpType: 5_telcom，5_union，5_bgp，5_sbgp.
// 东北-大连：5_telcom、5_union
// 华南-广州：5_sbgp
// 华东-上海二：5_sbgp
// 华北-北京一：5_bgp、5_sbgp
// 亚太-香港：5_bgp
func (self *SRegion) CreateEIP(eip *cloudprovider.SEip) (cloudprovider.ICloudEIP, error) {
	var ctype TInternetChargeType
	switch eip.ChargeType {
	case api.EIP_CHARGE_TYPE_BY_TRAFFIC:
		ctype = InternetChargeByTraffic
	case api.EIP_CHARGE_TYPE_BY_BANDWIDTH:
		ctype = InternetChargeByBandwidth
	}

	if len(eip.BGPType) == 0 {
		eip.BGPType = "5_bgp"
	}

	// 华为云EIP名字最大长度64
	if len(eip.Name) > 64 {
		eip.Name = eip.Name[:64]
	}

	ieip, err := self.AllocateEIP(eip.Name, eip.BandwidthMbps, ctype, eip.BGPType, eip.ProjectId)
	ieip.region = self
	if err != nil {
		return nil, err
	}

	err = cloudprovider.WaitStatus(ieip, api.EIP_STATUS_READY, 5*time.Second, 60*time.Second)
	return ieip, err
}

func (self *SRegion) GetISnapshots() ([]cloudprovider.ICloudSnapshot, error) {
	snapshots, err := self.GetSnapshots("", "")
	if err != nil {
		log.Errorf("self.GetSnapshots fail %s", err)
		return nil, err
	}

	ret := make([]cloudprovider.ICloudSnapshot, len(snapshots))
	for i := 0; i < len(snapshots); i += 1 {
		snapshots[i].region = self
		ret[i] = &snapshots[i]
	}
	return ret, nil
}

func (self *SRegion) GetISnapshotById(snapshotId string) (cloudprovider.ICloudSnapshot, error) {
	snapshot, err := self.GetSnapshotById(snapshotId)
	return &snapshot, err
}

func (self *SRegion) GetIHosts() ([]cloudprovider.ICloudHost, error) {
	iHosts := make([]cloudprovider.ICloudHost, 0)

	izones, err := self.GetIZones()
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(izones); i += 1 {
		iZoneHost, err := izones[i].GetIHosts()
		if err != nil {
			return nil, err
		}
		iHosts = append(iHosts, iZoneHost...)
	}
	return iHosts, nil
}

func (self *SRegion) GetIHostById(id string) (cloudprovider.ICloudHost, error) {
	izones, err := self.GetIZones()
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(izones); i += 1 {
		ihost, err := izones[i].GetIHostById(id)
		if err == nil {
			return ihost, nil
		} else if errors.Cause(err) != cloudprovider.ErrNotFound {
			return nil, err
		}
	}
	return nil, cloudprovider.ErrNotFound
}

func (self *SRegion) GetIStorages() ([]cloudprovider.ICloudStorage, error) {
	iStores := make([]cloudprovider.ICloudStorage, 0)

	izones, err := self.GetIZones()
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(izones); i += 1 {
		iZoneStores, err := izones[i].GetIStorages()
		if err != nil {
			return nil, err
		}
		iStores = append(iStores, iZoneStores...)
	}
	return iStores, nil
}

func (self *SRegion) GetIStorageById(id string) (cloudprovider.ICloudStorage, error) {
	izones, err := self.GetIZones()
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(izones); i += 1 {
		istore, err := izones[i].GetIStorageById(id)
		if err == nil {
			return istore, nil
		} else if errors.Cause(err) != cloudprovider.ErrNotFound {
			return nil, err
		}
	}
	return nil, cloudprovider.ErrNotFound
}

func (self *SRegion) GetProvider() string {
	return CLOUD_PROVIDER_HUAWEI
}

func (self *SRegion) GetCloudEnv() string {
	return ""
}

func (self *SRegion) CreateILoadBalancer(loadbalancer *cloudprovider.SLoadbalancerCreateOptions) (cloudprovider.ICloudLoadbalancer, error) {
	ret, err := self.CreateLoadBalancer(loadbalancer)
	if err != nil {
		return nil, err
	}

	return &ret, nil
}

// https://support.huaweicloud.com/api-elb/zh-cn_topic_0096561535.html
func (self *SRegion) CreateLoadBalancer(loadbalancer *cloudprovider.SLoadbalancerCreateOptions) (SLoadbalancer, error) {
	ret := SLoadbalancer{}
	subnet, err := self.getNetwork(loadbalancer.NetworkIds[0])
	if err != nil {
		return ret, errors.Wrap(err, "SRegion.CreateLoadBalancer.getNetwork")
	}

	params := jsonutils.NewDict()
	elbObj := jsonutils.NewDict()
	elbObj.Set("name", jsonutils.NewString(loadbalancer.Name))
	elbObj.Set("vip_subnet_id", jsonutils.NewString(subnet.NeutronSubnetID))
	if len(loadbalancer.Address) > 0 {
		elbObj.Set("vip_address", jsonutils.NewString(loadbalancer.Address))
	}
	elbObj.Set("tenant_id", jsonutils.NewString(self.client.projectId))
	params.Set("loadbalancer", elbObj)

	err = DoCreate(self.ecsClient.Elb.Create, params, &ret)
	if err != nil {
		return ret, errors.Wrap(err, "SRegion.CreateLoadBalancer.DoCreate")
	}

	ret.region = self

	// 创建公网类型ELB
	if len(loadbalancer.EipId) > 0 {
		err := self.AssociateEipWithPortId(loadbalancer.EipId, ret.VipPortID)
		if err != nil {
			return ret, errors.Wrap(err, "SRegion.CreateLoadBalancer.AssociateEipWithPortId")
		}
	}
	return ret, nil
}

func (self *SRegion) CreateILoadBalancerAcl(acl *cloudprovider.SLoadbalancerAccessControlList) (cloudprovider.ICloudLoadbalancerAcl, error) {
	ret, err := self.CreateLoadBalancerAcl(acl)
	if err != nil {
		return nil, err
	}

	return &ret, nil
}

func (self *SRegion) CreateLoadBalancerAcl(acl *cloudprovider.SLoadbalancerAccessControlList) (SElbACL, error) {
	params := jsonutils.NewDict()
	aclObj := jsonutils.NewDict()
	aclObj.Set("listener_id", jsonutils.NewString(acl.ListenerId))
	if len(acl.Entrys) > 0 {
		whitelist := []string{}
		for i := range acl.Entrys {
			whitelist = append(whitelist, acl.Entrys[i].CIDR)
		}

		aclObj.Set("enable_whitelist", jsonutils.NewBool(acl.AccessControlEnable))
		aclObj.Set("whitelist", jsonutils.NewString(strings.Join(whitelist, ",")))
	} else {
		aclObj.Set("enable_whitelist", jsonutils.NewBool(false))
	}
	params.Set("whitelist", aclObj)

	ret := SElbACL{}
	err := DoCreate(self.ecsClient.ElbWhitelist.Create, params, &ret)
	if err != nil {
		return ret, err
	}

	ret.region = self
	return ret, nil
}

func (region *SRegion) GetIBuckets() ([]cloudprovider.ICloudBucket, error) {
	iBuckets, err := region.client.getIBuckets()
	if err != nil {
		return nil, errors.Wrap(err, "getIBuckets")
	}
	ret := make([]cloudprovider.ICloudBucket, 0)
	for i := range iBuckets {
		// huawei OBS is shared across projects
		if iBuckets[i].GetLocation() == region.GetId() {
			ret = append(ret, iBuckets[i])
		}
	}
	return ret, nil
}

func str2StorageClass(storageClassStr string) (obs.StorageClassType, error) {
	if strings.EqualFold(storageClassStr, string(obs.StorageClassStandard)) {
		return obs.StorageClassStandard, nil
	} else if strings.EqualFold(storageClassStr, string(obs.StorageClassWarm)) {
		return obs.StorageClassWarm, nil
	} else if strings.EqualFold(storageClassStr, string(obs.StorageClassCold)) {
		return obs.StorageClassCold, nil
	} else {
		return obs.StorageClassStandard, errors.Error("unsupported storageClass")
	}
}

func (region *SRegion) CreateIBucket(name string, storageClassStr string, aclStr string) error {
	obsClient, err := region.getOBSClient()
	if err != nil {
		return errors.Wrap(err, "region.getOBSClient")
	}
	input := &obs.CreateBucketInput{}
	input.Bucket = name
	input.Location = region.GetId()
	if len(aclStr) > 0 {
		if strings.EqualFold(aclStr, string(obs.AclPrivate)) {
			input.ACL = obs.AclPrivate
		} else if strings.EqualFold(aclStr, string(obs.AclPublicRead)) {
			input.ACL = obs.AclPublicRead
		} else if strings.EqualFold(aclStr, string(obs.AclPublicReadWrite)) {
			input.ACL = obs.AclPublicReadWrite
		} else {
			return errors.Error("unsupported acl")
		}
	}
	if len(storageClassStr) > 0 {
		input.StorageClass, err = str2StorageClass(storageClassStr)
		if err != nil {
			return err
		}
	}
	_, err = obsClient.CreateBucket(input)
	if err != nil {
		return errors.Wrap(err, "obsClient.CreateBucket")
	}
	region.client.invalidateIBuckets()
	return nil
}

func obsHttpCode(err error) int {
	switch httpErr := err.(type) {
	case obs.ObsError:
		return httpErr.StatusCode
	case *obs.ObsError:
		return httpErr.StatusCode
	}
	return -1
}

func (region *SRegion) DeleteIBucket(name string) error {
	obsClient, err := region.getOBSClient()
	if err != nil {
		return errors.Wrap(err, "region.getOBSClient")
	}
	_, err = obsClient.DeleteBucket(name)
	if err != nil {
		if obsHttpCode(err) == 404 {
			return nil
		}
		log.Debugf("%#v %s", err, err)
		return errors.Wrap(err, "DeleteBucket")
	}
	region.client.invalidateIBuckets()
	return nil
}

func (region *SRegion) HeadBucket(name string) (*obs.BaseModel, error) {
	obsClient, err := region.getOBSClient()
	if err != nil {
		return nil, errors.Wrap(err, "region.getOBSClient")
	}
	return obsClient.HeadBucket(name)
}

func (region *SRegion) IBucketExist(name string) (bool, error) {
	_, err := region.HeadBucket(name)
	if err != nil {
		if obsHttpCode(err) == 404 {
			return false, nil
		} else {
			return false, errors.Wrap(err, "HeadBucket")
		}
	}
	return true, nil
}

func (region *SRegion) GetIBucketById(name string) (cloudprovider.ICloudBucket, error) {
	return cloudprovider.GetIBucketById(region, name)
}

func (region *SRegion) GetIBucketByName(name string) (cloudprovider.ICloudBucket, error) {
	return region.GetIBucketById(name)
}

func (self *SRegion) GetSkus(zoneId string) ([]cloudprovider.ICloudSku, error) {
	if self.iskus != nil {
		return self.iskus, nil
	}

	ret := make([]cloudprovider.ICloudSku, 0)
	flavors, err := self.fetchInstanceTypes(zoneId)
	if err != nil {
		return nil, errors.Wrap(err, "fetchInstanceTypes")
	}

	for i := range flavors {
		ret = append(ret, &flavors[i])
	}

	self.iskus = ret
	return ret, nil
}

func (self *SRegion) GetICloudSku(skuId string) (cloudprovider.ICloudSku, error) {
	skus, err := self.GetSkus("")
	if err != nil {
		return nil, err
	}

	for i := range skus {
		if skus[i].GetId() == skuId {
			return skus[i], nil
		}
	}

	return nil, errors.Wrap(cloudprovider.ErrNotFound, "GetICloudSku")
}

func (self *SRegion) GetIElasticcaches() ([]cloudprovider.ICloudElasticcache, error) {
	caches, err := self.GetElasticCaches()
	if err != nil {
		return nil, err
	}

	icaches := make([]cloudprovider.ICloudElasticcache, len(caches))
	for i := range caches {
		caches[i].region = self
		icaches[i] = &caches[i]
	}

	return icaches, nil
}

func (region *SRegion) GetCapabilities() []string {
	return region.client.GetCapabilities()
}

func (self *SRegion) GetDiskTypes() ([]SDiskType, error) {
	ret, err := self.ecsClient.Disks.GetDiskTypes()
	if err != nil {
		return nil, errors.Wrap(err, "GetDiskTypes")
	}

	dts := []SDiskType{}
	_ret := jsonutils.NewArray(ret.Data...)
	err = _ret.Unmarshal(&dts)
	if err != nil {
		return nil, errors.Wrap(err, "Unmarshal")
	}

	return dts, nil
}

func (self *SRegion) GetZoneSupportedDiskTypes(zoneId string) ([]string, error) {
	dts, err := self.GetDiskTypes()
	if err != nil {
		return nil, errors.Wrap(err, "GetDiskTypes")
	}

	ret := []string{}
	for i := range dts {
		if dts[i].IsAvaliableInZone(zoneId) {
			ret = append(ret, dts[i].Name)
		}
	}

	return ret, nil
}

func (self *SRegion) GetISkus() ([]cloudprovider.ICloudSku, error) {
	return self.GetSkus("")
}

func (self *SRegion) GetEndpoints() ([]jsonutils.JSONObject, error) {
	endpoints := make([]jsonutils.JSONObject, 0)
	err := doListAll(self.ecsClient.Endpoints.List, nil, &endpoints)
	if err != nil {
		return nil, err
	}

	return endpoints, nil
}

func (self *SRegion) GetServices() ([]jsonutils.JSONObject, error) {
	services := make([]jsonutils.JSONObject, 0)
	err := doListAll(self.ecsClient.Services.List, nil, &services)
	if err != nil {
		return nil, err
	}

	return services, nil
}

func (region *SRegion) GetIVMs() ([]cloudprovider.ICloudVM, error) {
	vms, err := region.GetInstances()
	if err != nil {
		return nil, errors.Wrap(err, "GetInstances")
	}
	ret := []cloudprovider.ICloudVM{}
	for i := range vms {
		ret = append(ret, &vms[i])
	}
	return ret, nil
}
