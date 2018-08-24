/*
 * Minio Cloud Storage, (C) 2018 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"path"
	"runtime"
	"sync"
	"time"

	"github.com/minio/minio/pkg/auth"
	"github.com/minio/minio/pkg/iam"
	"github.com/minio/minio/pkg/iam/policy"
	"github.com/minio/minio/pkg/iam/users"
	"github.com/minio/minio/pkg/iam/validator"
	"github.com/minio/minio/pkg/quick"
)

const (
	// IAM configuration directory.
	iamConfigPrefix = minioConfigPrefix + "/iam"

	// IAM configuration file.
	iamConfigFile = "iam.json"
)

// STS handler global values
var (
	// Authorization validators list.
	globalIAMValidators *validator.Validators

	// Global IAM config.
	globalIAMConfig *iam.IAM

	// Global mutex to update validators list.
	globalIAMValidatorsMu sync.RWMutex
)

func saveIAMConfig(ctx context.Context, objAPI ObjectLayer, cfg *iam.IAM) error {
	if err := quick.CheckData(cfg); err != nil {
		return err
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}

	configFile := path.Join(iamConfigPrefix, iamConfigFile)
	if globalEtcdClient != nil {
		_, err := globalEtcdClient.Put(ctx, configFile, string(data))
		return err
	}

	return saveConfig(ctx, objAPI, configFile, data)
}

func readIAMConfig(ctx context.Context, objAPI ObjectLayer) (*iam.IAM, error) {
	var configData []byte
	var err error
	configFile := path.Join(iamConfigPrefix, iamConfigFile)
	if globalEtcdClient != nil {
		configData, err = readConfigEtcd(ctx, globalEtcdClient, configFile)
	} else {
		configData, err = readConfig(ctx, objAPI, configFile)
	}
	if err != nil {
		return nil, err
	}

	if runtime.GOOS == "windows" {
		configData = bytes.Replace(configData, []byte("\r\n"), []byte("\n"), -1)
	}

	if err = quick.CheckDuplicateKeys(string(configData)); err != nil {
		return nil, err
	}

	var config = &iam.IAM{}
	if err = json.Unmarshal(configData, config); err != nil {
		return nil, err
	}

	if err = quick.CheckData(config); err != nil {
		return nil, err
	}

	return config, nil
}

// loadIAMConfig - loads new IAM config from disk.
func loadIAMConfig(objAPI ObjectLayer) error {
	iamCfg, err := readIAMConfig(context.Background(), objAPI)
	if err != nil {
		return err
	}

	// hold the mutex lock before a new config is assigned.
	globalIAMValidatorsMu.Lock()
	defer globalIAMValidatorsMu.Unlock()

	globalIAMConfig = iamCfg
	globalIAMValidators = iamCfg.GetAuthValidators()

	return nil
}

func initIAMConfig(objAPI ObjectLayer) error {
	if objAPI == nil {
		return errServerNotInitialized
	}

	configFile := path.Join(iamConfigPrefix, iamConfigFile)

	err := checkConfig(context.Background(), objAPI, configFile)
	if err != nil && err != errConfigNotFound {
		return err
	}

	if err == errConfigNotFound {
		var iamCfg *iam.IAM
		// IAM config does not exist, we create it fresh and return upon success.
		iamCfg, err = newIAMConfig()
		if err != nil {
			return err
		}
		// hold the mutex lock before a new config is assigned.
		globalIAMValidatorsMu.Lock()
		defer globalIAMValidatorsMu.Unlock()
		globalIAMConfig = iamCfg
		globalIAMValidators = iamCfg.GetAuthValidators()
		return saveIAMConfig(context.Background(), objAPI, iamCfg)
	}

	go watchConfig(objAPI, configFile, loadIAMConfig)

	return loadIAMConfig(objAPI)
}

// newIAMConfig - initializes a new IAM config.
func newIAMConfig() (*iam.IAM, error) {
	return iam.New()
}

// IAMSys - config system.
type IAMSys struct {
	sync.RWMutex
	iamPolicyOPA *iampolicy.Opa
	iamEtcdUsers *iamusers.Store
	iamUsersMap  map[string]auth.Credentials
	iamPolicyMap map[string]iampolicy.Policy
}

// Load - load iam.json
func (sys *IAMSys) Load(objAPI ObjectLayer) error {
	return sys.Init(objAPI)
}

// Init - initializes config system from iam.json
func (sys *IAMSys) Init(objAPI ObjectLayer) error {
	if objAPI == nil {
		return errInvalidArgument
	}

	if err := initIAMConfig(objAPI); err != nil {
		return err
	}

	if globalIAMConfig.Policy.Type == iam.PolicyOPA {
		sys.iamPolicyOPA = iampolicy.NewOpa(globalIAMConfig.Policy.OPA)
	}

	if globalEtcdClient == nil && globalIAMConfig.Identity.Type == iam.IAMOpenID {
		return errInvalidArgument
	}

	if globalEtcdClient != nil {
		var err error
		sys.iamEtcdUsers, err = iamusers.NewEtcdStore(globalEtcdClient)
		if err != nil {
			return err
		}
	}

	if err := sys.refresh(objAPI); err != nil {
		return err
	}

	// Refresh IAMSys in background.
	go func() {
		ticker := time.NewTicker(globalRefreshIAMInterval)
		defer ticker.Stop()
		for {
			select {
			case <-globalServiceDoneCh:
				return
			case <-ticker.C:
				sys.refresh(objAPI)
			}
		}
	}()

	return nil

}

// SetPolicy - sets policy to given user name.  If policy is empty,
// existing policy is removed.
func (sys *IAMSys) SetPolicy(accountName string, p iampolicy.Policy) {
	sys.Lock()
	defer sys.Unlock()

	if p.IsEmpty() {
		delete(sys.iamPolicyMap, accountName)
	} else {
		sys.iamPolicyMap[accountName] = p
	}
}

// RemovePolicy - removes policy for given account name.
func (sys *IAMSys) RemovePolicy(accountName string) {
	sys.Lock()
	defer sys.Unlock()

	delete(sys.iamPolicyMap, accountName)
}

// SetUser - set user credentials.
func (sys *IAMSys) SetUser(accountName string, cred auth.Credentials) error {
	if sys.iamEtcdUsers != nil {
		if err := sys.iamEtcdUsers.Set(cred); err != nil {
			return err
		}
	}

	sys.Lock()
	defer sys.Unlock()

	sys.iamUsersMap[accountName] = cred
	return nil
}

// GetUser - get user credentials
func (sys *IAMSys) GetUser(accountName string) (auth.Credentials, bool) {
	if sys.iamEtcdUsers != nil {
		cred, ok := sys.iamEtcdUsers.Get(accountName)
		if ok {
			return cred, ok
		}
	}

	sys.RLock()
	defer sys.RUnlock()

	cred, ok := sys.iamUsersMap[accountName]
	return cred, ok
}

// IsAllowed - checks given policy args is allowed to continue the Rest API.
func (sys *IAMSys) IsAllowed(args iampolicy.Args) bool {
	// If opa is configured, let the policy arrive from Opa
	if sys.iamPolicyOPA != nil {
		return sys.iamPolicyOPA.IsAllowed(args)
	}

	sys.RLock()
	defer sys.RUnlock()

	// If policy is available for given user, check the policy.
	if p, found := sys.iamPolicyMap[args.AccountName]; found {
		return p.IsAllowed(args)
	}

	// As policy is not available for given bucket name, returns IsOwner i.e.
	// operation is allowed only for owner.
	return args.IsOwner
}

// Refresh IAMSys.
func (sys *IAMSys) refresh(objAPI ObjectLayer) error {
	iamCfg, err := readIAMConfig(context.Background(), objAPI)
	if err != nil {
		return err
	}
	for k, v := range iamCfg.Policy.Minio.Users {
		sys.SetPolicy(k, v)
	}
	for k, v := range iamCfg.Identity.Minio.Users {
		sys.SetUser(k, v)
	}
	return nil
}

// NewIAMSys - creates new config system object.
func NewIAMSys() *IAMSys {
	return &IAMSys{
		iamUsersMap:  make(map[string]auth.Credentials),
		iamPolicyMap: make(map[string]iampolicy.Policy),
	}
}
