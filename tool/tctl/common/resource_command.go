/*
Copyright 2015-2021 Gravitational, Inc.

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

package common

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/gravitational/kingpin"
	"github.com/gravitational/trace"
	"github.com/gravitational/trace/trail"
	log "github.com/sirupsen/logrus"
	"golang.org/x/exp/slices"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/gravitational/teleport"
	apiclient "github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	devicepb "github.com/gravitational/teleport/api/gen/proto/go/teleport/devicetrust/v1"
	loginrulepb "github.com/gravitational/teleport/api/gen/proto/go/teleport/loginrule/v1"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/api/types/installers"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/devicetrust"
	"github.com/gravitational/teleport/lib/service/servicecfg"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/teleport/tool/tctl/common/device"
	"github.com/gravitational/teleport/tool/tctl/common/loginrule"
)

// ResourceCreateHandler is the generic implementation of a resource creation handler
type ResourceCreateHandler func(context.Context, auth.ClientI, services.UnknownResource) error

// ResourceKind is the string form of a resource, i.e. "oidc"
type ResourceKind string

// ResourceCommand implements `tctl get/create/list` commands for manipulating
// Teleport resources
type ResourceCommand struct {
	config      *servicecfg.Config
	ref         services.Ref
	refs        services.Refs
	format      string
	namespace   string
	withSecrets bool
	force       bool
	confirm     bool
	ttl         string
	labels      string

	// filename is the name of the resource, used for 'create'
	filename string

	// CLI subcommands:
	deleteCmd *kingpin.CmdClause
	getCmd    *kingpin.CmdClause
	createCmd *kingpin.CmdClause
	updateCmd *kingpin.CmdClause

	verbose bool

	CreateHandlers map[ResourceKind]ResourceCreateHandler

	// stdout allows to switch standard output source for resource command. Used in tests.
	stdout io.Writer
}

const getHelp = `Examples:

  $ tctl get clusters       : prints the list of all trusted clusters
  $ tctl get cluster/east   : prints the trusted cluster 'east'
  $ tctl get clusters,users : prints all trusted clusters and all users

Same as above, but using JSON output:

  $ tctl get clusters --format=json
`

// Initialize allows ResourceCommand to plug itself into the CLI parser
func (rc *ResourceCommand) Initialize(app *kingpin.Application, config *servicecfg.Config) {
	rc.CreateHandlers = map[ResourceKind]ResourceCreateHandler{
		types.KindUser:                    rc.createUser,
		types.KindRole:                    rc.createRole,
		types.KindTrustedCluster:          rc.createTrustedCluster,
		types.KindGithubConnector:         rc.createGithubConnector,
		types.KindCertAuthority:           rc.createCertAuthority,
		types.KindClusterAuthPreference:   rc.createAuthPreference,
		types.KindClusterNetworkingConfig: rc.createClusterNetworkingConfig,
		types.KindSessionRecordingConfig:  rc.createSessionRecordingConfig,
		types.KindUIConfig:                rc.createUIConfig,
		types.KindLock:                    rc.createLock,
		types.KindNetworkRestrictions:     rc.createNetworkRestrictions,
		types.KindApp:                     rc.createApp,
		types.KindDatabase:                rc.createDatabase,
		types.KindKubernetesCluster:       rc.createKubeCluster,
		types.KindToken:                   rc.createToken,
		types.KindInstaller:               rc.createInstaller,
		types.KindNode:                    rc.createNode,
		types.KindOIDCConnector:           rc.createOIDCConnector,
		types.KindSAMLConnector:           rc.createSAMLConnector,
		types.KindLoginRule:               rc.createLoginRule,
		types.KindSAMLIdPServiceProvider:  rc.createSAMLIdPServiceProvider,
		types.KindDevice:                  rc.createDevice,
	}
	rc.config = config

	rc.createCmd = app.Command("create", "Create or update a Teleport resource from a YAML file")
	rc.createCmd.Arg("filename", "resource definition file, empty for stdin").StringVar(&rc.filename)
	rc.createCmd.Flag("force", "Overwrite the resource if already exists").Short('f').BoolVar(&rc.force)
	rc.createCmd.Flag("confirm", "Confirm an unsafe or temporary resource update").Hidden().BoolVar(&rc.confirm)

	rc.updateCmd = app.Command("update", "Update resource fields")
	rc.updateCmd.Arg("resource type/resource name", `Resource to update
	<resource type>  Type of a resource [for example: rc]
	<resource name>  Resource name to update

	Example:
	$ tctl update rc/remote`).SetValue(&rc.ref)
	rc.updateCmd.Flag("set-labels", "Set labels").StringVar(&rc.labels)
	rc.updateCmd.Flag("set-ttl", "Set TTL").StringVar(&rc.ttl)

	rc.deleteCmd = app.Command("rm", "Delete a resource").Alias("del")
	rc.deleteCmd.Arg("resource type/resource name", `Resource to delete
	<resource type>  Type of a resource [for example: connector,user,cluster,token]
	<resource name>  Resource name to delete

	Examples:
	$ tctl rm connector/github
	$ tctl rm cluster/main`).SetValue(&rc.ref)

	rc.getCmd = app.Command("get", "Print a YAML declaration of various Teleport resources")
	rc.getCmd.Arg("resources", "Resource spec: 'type/[name][,...]' or 'all'").Required().SetValue(&rc.refs)
	rc.getCmd.Flag("format", "Output format: 'yaml', 'json' or 'text'").Default(teleport.YAML).StringVar(&rc.format)
	rc.getCmd.Flag("namespace", "Namespace of the resources").Hidden().Default(apidefaults.Namespace).StringVar(&rc.namespace)
	rc.getCmd.Flag("with-secrets", "Include secrets in resources like certificate authorities or OIDC connectors").Default("false").BoolVar(&rc.withSecrets)
	rc.getCmd.Flag("verbose", "Verbose table output, shows full label output").Short('v').BoolVar(&rc.verbose)

	rc.getCmd.Alias(getHelp)

	if rc.stdout == nil {
		rc.stdout = os.Stdout
	}
}

// TryRun takes the CLI command as an argument (like "auth gen") and executes it
// or returns match=false if 'cmd' does not belong to it
func (rc *ResourceCommand) TryRun(ctx context.Context, cmd string, client auth.ClientI) (match bool, err error) {
	switch cmd {
	// tctl get
	case rc.getCmd.FullCommand():
		err = rc.Get(ctx, client)
		// tctl create
	case rc.createCmd.FullCommand():
		err = rc.Create(ctx, client)
		// tctl rm
	case rc.deleteCmd.FullCommand():
		err = rc.Delete(ctx, client)
		// tctl update
	case rc.updateCmd.FullCommand():
		err = rc.Update(ctx, client)
	default:
		return false, nil
	}
	return true, trace.Wrap(err)
}

// IsDeleteSubcommand returns 'true' if the given command is `tctl rm`
func (rc *ResourceCommand) IsDeleteSubcommand(cmd string) bool {
	return cmd == rc.deleteCmd.FullCommand()
}

// GetRef returns the reference (basically type/name pair) of the resource
// the command is operating on
func (rc *ResourceCommand) GetRef() services.Ref {
	return rc.ref
}

// Get prints one or many resources of a certain type
func (rc *ResourceCommand) Get(ctx context.Context, client auth.ClientI) error {
	if rc.refs.IsAll() {
		return rc.GetAll(ctx, client)
	}
	if len(rc.refs) != 1 {
		return rc.GetMany(ctx, client)
	}
	rc.ref = rc.refs[0]
	collection, err := rc.getCollection(ctx, client)
	if err != nil {
		return trace.Wrap(err)
	}

	// Note that only YAML is officially supported. Support for text and JSON
	// is experimental.
	switch rc.format {
	case teleport.Text:
		return collection.writeText(rc.stdout)
	case teleport.YAML:
		return writeYAML(collection, rc.stdout)
	case teleport.JSON:
		return writeJSON(collection, rc.stdout)
	}
	return trace.BadParameter("unsupported format")
}

func (rc *ResourceCommand) GetMany(ctx context.Context, client auth.ClientI) error {
	if rc.format != teleport.YAML {
		return trace.BadParameter("mixed resource types only support YAML formatting")
	}
	var resources []types.Resource
	for _, ref := range rc.refs {
		rc.ref = ref
		collection, err := rc.getCollection(ctx, client)
		if err != nil {
			return trace.Wrap(err)
		}
		resources = append(resources, collection.resources()...)
	}
	if err := utils.WriteYAML(os.Stdout, resources); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func (rc *ResourceCommand) GetAll(ctx context.Context, client auth.ClientI) error {
	rc.withSecrets = true
	allKinds := services.GetResourceMarshalerKinds()
	allRefs := make([]services.Ref, 0, len(allKinds))
	for _, kind := range allKinds {
		ref := services.Ref{
			Kind: kind,
		}
		allRefs = append(allRefs, ref)
	}
	rc.refs = services.Refs(allRefs)
	return rc.GetMany(ctx, client)
}

// Create updates or inserts one or many resources
func (rc *ResourceCommand) Create(ctx context.Context, client auth.ClientI) (err error) {
	var reader io.Reader
	if rc.filename == "" {
		reader = os.Stdin
	} else {
		f, err := utils.OpenFile(rc.filename)
		if err != nil {
			return trace.Wrap(err)
		}
		defer f.Close()
		reader = f
	}
	decoder := kyaml.NewYAMLOrJSONDecoder(reader, defaults.LookaheadBufSize)
	count := 0
	for {
		var raw services.UnknownResource
		err := decoder.Decode(&raw)
		if err != nil {
			if err == io.EOF {
				if count == 0 {
					return trace.BadParameter("no resources found, empty input?")
				}
				return nil
			}
			return trace.Wrap(err)
		}
		count++

		// locate the creator function for a given resource kind:
		creator, found := rc.CreateHandlers[ResourceKind(raw.Kind)]
		if !found {
			return trace.BadParameter("creating resources of type %q is not supported", raw.Kind)
		}
		// only return in case of error, to create multiple resources
		// in case if yaml spec is a list
		if err := creator(ctx, client, raw); err != nil {
			if trace.IsAlreadyExists(err) {
				return trace.Wrap(err, "use -f or --force flag to overwrite")
			}
			return trace.Wrap(err)
		}
	}
}

// createTrustedCluster implements `tctl create cluster.yaml` command
func (rc *ResourceCommand) createTrustedCluster(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	tc, err := services.UnmarshalTrustedCluster(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}

	// check if such cluster already exists:
	name := tc.GetName()
	_, err = client.GetTrustedCluster(ctx, name)
	if err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}

	exists := (err == nil)
	if !rc.force && exists {
		return trace.AlreadyExists("trusted cluster %q already exists", name)
	}

	out, err := client.UpsertTrustedCluster(ctx, tc)
	if err != nil {
		// If force is used and UpsertTrustedCluster returns trace.AlreadyExists,
		// this means the user tried to upsert a cluster whose exact match already
		// exists in the backend, nothing needs to occur other than happy message
		// that the trusted cluster has been created.
		if rc.force && trace.IsAlreadyExists(err) {
			out = tc
		} else {
			return trace.Wrap(err)
		}
	}
	if out.GetName() != tc.GetName() {
		fmt.Printf("WARNING: trusted cluster %q resource has been renamed to match remote cluster name %q\n", name, out.GetName())
	}
	fmt.Printf("trusted cluster %q has been %v\n", out.GetName(), UpsertVerb(exists, rc.force))
	return nil
}

// createCertAuthority creates certificate authority
func (rc *ResourceCommand) createCertAuthority(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	certAuthority, err := services.UnmarshalCertAuthority(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}
	if err := client.UpsertCertAuthority(ctx, certAuthority); err != nil {
		return trace.Wrap(err)
	}
	fmt.Printf("certificate authority '%s' has been updated\n", certAuthority.GetName())
	return nil
}

// createGithubConnector creates a Github connector
func (rc *ResourceCommand) createGithubConnector(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	connector, err := services.UnmarshalGithubConnector(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}
	_, err = client.GetGithubConnector(ctx, connector.GetName(), false)
	if err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	exists := (err == nil)
	if !rc.force && exists {
		return trace.AlreadyExists("authentication connector %q already exists",
			connector.GetName())
	}
	err = client.UpsertGithubConnector(ctx, connector)
	if err != nil {
		return trace.Wrap(err)
	}
	fmt.Printf("authentication connector %q has been %s\n",
		connector.GetName(), UpsertVerb(exists, rc.force))
	return nil
}

// createRole implements `tctl create role.yaml` command.
func (rc *ResourceCommand) createRole(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	role, err := services.UnmarshalRole(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}
	err = role.CheckAndSetDefaults()
	if err != nil {
		return trace.Wrap(err)
	}

	if err := services.ValidateAccessPredicates(role); err != nil {
		// check for syntax errors in predicates
		return trace.Wrap(err)
	}

	roleName := role.GetName()
	_, err = client.GetRole(ctx, roleName)
	if err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	roleExists := (err == nil)
	if roleExists && !rc.IsForced() {
		return trace.AlreadyExists("role '%s' already exists", roleName)
	}
	if err := client.UpsertRole(ctx, role); err != nil {
		return trace.Wrap(err)
	}
	fmt.Printf("role '%s' has been %s\n", roleName, UpsertVerb(roleExists, rc.IsForced()))
	return nil
}

// createUser implements `tctl create user.yaml` command.
func (rc *ResourceCommand) createUser(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	user, err := services.UnmarshalUser(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}

	userName := user.GetName()
	existingUser, err := client.GetUser(userName, false)
	if err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	exists := (err == nil)

	if exists {
		if !rc.force {
			return trace.AlreadyExists("user %q already exists", userName)
		}

		// Unmarshalling user sets createdBy to zero values which will overwrite existing data.
		// This field should not be allowed to be overwritten.
		user.SetCreatedBy(existingUser.GetCreatedBy())

		if err := client.UpdateUser(ctx, user); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("user %q has been updated\n", userName)

	} else {
		if err := client.CreateUser(ctx, user); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("user %q has been created\n", userName)
	}

	return nil
}

// createAuthPreference implements `tctl create cap.yaml` command.
func (rc *ResourceCommand) createAuthPreference(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	newAuthPref, err := services.UnmarshalAuthPreference(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}

	storedAuthPref, err := client.GetAuthPreference(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	if err := checkCreateResourceWithOrigin(storedAuthPref, "cluster auth preference", rc.force, rc.confirm); err != nil {
		return trace.Wrap(err)
	}

	if err := client.SetAuthPreference(ctx, newAuthPref); err != nil {
		return trace.Wrap(err)
	}
	fmt.Printf("cluster auth preference has been updated\n")
	return nil
}

// createClusterNetworkingConfig implements `tctl create netconfig.yaml` command.
func (rc *ResourceCommand) createClusterNetworkingConfig(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	newNetConfig, err := services.UnmarshalClusterNetworkingConfig(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}

	storedNetConfig, err := client.GetClusterNetworkingConfig(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	if err := checkCreateResourceWithOrigin(storedNetConfig, "cluster networking configuration", rc.force, rc.confirm); err != nil {
		return trace.Wrap(err)
	}

	if err := client.SetClusterNetworkingConfig(ctx, newNetConfig); err != nil {
		return trace.Wrap(err)
	}
	fmt.Printf("cluster networking configuration has been updated\n")
	return nil
}

// createSessionRecordingConfig implements `tctl create recconfig.yaml` command.
func (rc *ResourceCommand) createSessionRecordingConfig(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	newRecConfig, err := services.UnmarshalSessionRecordingConfig(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}

	storedRecConfig, err := client.GetSessionRecordingConfig(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	if err := checkCreateResourceWithOrigin(storedRecConfig, "session recording configuration", rc.force, rc.confirm); err != nil {
		return trace.Wrap(err)
	}

	if err := client.SetSessionRecordingConfig(ctx, newRecConfig); err != nil {
		return trace.Wrap(err)
	}
	fmt.Printf("session recording configuration has been updated\n")
	return nil
}

// createLock implements `tctl create lock.yaml` command.
func (rc *ResourceCommand) createLock(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	lock, err := services.UnmarshalLock(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}

	// Check if a lock of the name already exists.
	name := lock.GetName()
	_, err = client.GetLock(ctx, name)
	if err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}

	exists := (err == nil)
	if !rc.force && exists {
		return trace.AlreadyExists("lock %q already exists", name)
	}

	if err := client.UpsertLock(ctx, lock); err != nil {
		return trace.Wrap(err)
	}
	fmt.Printf("lock %q has been %s\n", name, UpsertVerb(exists, rc.force))
	return nil
}

// createNetworkRestrictions implements `tctl create net_restrict.yaml` command.
func (rc *ResourceCommand) createNetworkRestrictions(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	newNetRestricts, err := services.UnmarshalNetworkRestrictions(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}

	if err := client.SetNetworkRestrictions(ctx, newNetRestricts); err != nil {
		return trace.Wrap(err)
	}
	fmt.Printf("network restrictions have been updated\n")
	return nil
}

func (rc *ResourceCommand) createApp(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	app, err := services.UnmarshalApp(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}
	if err := client.CreateApp(ctx, app); err != nil {
		if trace.IsAlreadyExists(err) {
			if !rc.force {
				return trace.AlreadyExists("application %q already exists", app.GetName())
			}
			if err := client.UpdateApp(ctx, app); err != nil {
				return trace.Wrap(err)
			}
			fmt.Printf("application %q has been updated\n", app.GetName())
			return nil
		}
		return trace.Wrap(err)
	}
	fmt.Printf("application %q has been created\n", app.GetName())
	return nil
}

func (rc *ResourceCommand) createKubeCluster(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	cluster, err := services.UnmarshalKubeCluster(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}
	if err := client.CreateKubernetesCluster(ctx, cluster); err != nil {
		if trace.IsAlreadyExists(err) {
			if !rc.force {
				return trace.AlreadyExists("kubernetes cluster %q already exists", cluster.GetName())
			}
			if err := client.UpdateKubernetesCluster(ctx, cluster); err != nil {
				return trace.Wrap(err)
			}
			fmt.Printf("kubernetes cluster %q has been updated\n", cluster.GetName())
			return nil
		}
		return trace.Wrap(err)
	}
	fmt.Printf("kubernetes cluster %q has been created\n", cluster.GetName())
	return nil
}

func (rc *ResourceCommand) createDatabase(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	database, err := services.UnmarshalDatabase(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}
	database.SetOrigin(types.OriginDynamic)
	if err := client.CreateDatabase(ctx, database); err != nil {
		if trace.IsAlreadyExists(err) {
			if !rc.force {
				return trace.AlreadyExists("database %q already exists", database.GetName())
			}
			if err := client.UpdateDatabase(ctx, database); err != nil {
				return trace.Wrap(err)
			}
			fmt.Printf("database %q has been updated\n", database.GetName())
			return nil
		}
		return trace.Wrap(err)
	}
	fmt.Printf("database %q has been created\n", database.GetName())
	return nil
}

func (rc *ResourceCommand) createToken(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	token, err := services.UnmarshalProvisionToken(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}

	err = client.UpsertToken(ctx, token)
	return trace.Wrap(err)
}

func (rc *ResourceCommand) createInstaller(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	inst, err := services.UnmarshalInstaller(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}

	err = client.SetInstaller(ctx, inst)
	return trace.Wrap(err)
}

func (rc *ResourceCommand) createUIConfig(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	uic, err := services.UnmarshalUIConfig(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}

	return trace.Wrap(client.SetUIConfig(ctx, uic))
}

func (rc *ResourceCommand) createNode(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	server, err := services.UnmarshalServer(raw.Raw, types.KindNode)
	if err != nil {
		return trace.Wrap(err)
	}
	if err := server.CheckAndSetDefaults(); err != nil {
		return trace.Wrap(err)
	}

	name := server.GetName()
	_, err = client.GetNode(ctx, server.GetNamespace(), name)
	if err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	exists := (err == nil)
	if !rc.IsForced() && exists {
		return trace.AlreadyExists("node %q with Hostname %q and Addr %q already exists, use --force flag to override",
			name,
			server.GetHostname(),
			server.GetAddr(),
		)
	}

	_, err = client.UpsertNode(ctx, server)
	return trace.Wrap(err)
}

func (rc *ResourceCommand) createOIDCConnector(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	conn, err := services.UnmarshalOIDCConnector(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}
	if err := conn.CheckAndSetDefaults(); err != nil {
		return trace.Wrap(err)
	}

	connectorName := conn.GetName()
	_, err = client.GetOIDCConnector(ctx, connectorName, false)
	if err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	exists := (err == nil)
	if !rc.IsForced() && exists {
		return trace.AlreadyExists("connector '%s' already exists, use -f flag to override", connectorName)
	}
	if err = client.UpsertOIDCConnector(ctx, conn); err != nil {
		return trace.Wrap(err)
	}
	fmt.Printf("authentication connector '%s' has been %s\n", connectorName, UpsertVerb(exists, rc.IsForced()))
	return nil
}

func (rc *ResourceCommand) createSAMLConnector(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	// Create services.SAMLConnector from raw YAML to extract the connector name.
	conn, err := services.UnmarshalSAMLConnector(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}

	connectorName := conn.GetName()
	foundConn, err := client.GetSAMLConnector(ctx, connectorName, true)
	if err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	exists := (err == nil)
	if !rc.IsForced() && exists {
		return trace.AlreadyExists("connector '%s' already exists, use -f flag to override", connectorName)
	}

	// If the connector being pushed to the backend does not have a signing key
	// in it and an existing connector was found in the backend, extract the
	// signing key from the found connector and inject it into the connector
	// being injected into the backend.
	if conn.GetSigningKeyPair() == nil && exists {
		conn.SetSigningKeyPair(foundConn.GetSigningKeyPair())
	}
	if err := conn.CheckAndSetDefaults(); err != nil {
		return trace.Wrap(err)
	}

	if err = client.UpsertSAMLConnector(ctx, conn); err != nil {
		return trace.Wrap(err)
	}
	fmt.Printf("authentication connector '%s' has been %s\n", connectorName, UpsertVerb(exists, rc.IsForced()))
	return nil
}

func (rc *ResourceCommand) createLoginRule(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	rule, err := loginrule.UnmarshalLoginRule(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}

	loginRuleClient := client.LoginRuleClient()
	if rc.IsForced() {
		_, err := loginRuleClient.UpsertLoginRule(ctx, &loginrulepb.UpsertLoginRuleRequest{
			LoginRule: rule,
		})
		return trail.FromGRPC(err)
	}
	_, err = loginRuleClient.CreateLoginRule(ctx, &loginrulepb.CreateLoginRuleRequest{
		LoginRule: rule,
	})
	return trail.FromGRPC(err)
}

func (rc *ResourceCommand) createSAMLIdPServiceProvider(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	// Create services.SAMLIdPServiceProvider from raw YAML to extract the service provider name.
	sp, err := services.UnmarshalSAMLIdPServiceProvider(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}

	serviceProviderName := sp.GetName()
	if err := sp.CheckAndSetDefaults(); err != nil {
		return trace.Wrap(err)
	}

	exists := false
	if err = client.CreateSAMLIdPServiceProvider(ctx, sp); err != nil {
		if trace.IsAlreadyExists(err) {
			exists = true
			err = client.UpdateSAMLIdPServiceProvider(ctx, sp)
		}

		if err != nil {
			return trace.Wrap(err)
		}
	}
	fmt.Printf("SAML IdP service provider '%s' has been %s\n", serviceProviderName, UpsertVerb(exists, rc.IsForced()))
	return nil
}

func (rc *ResourceCommand) createDevice(ctx context.Context, client auth.ClientI, raw services.UnknownResource) error {
	if rc.IsForced() {
		fmt.Printf("Warning: Devices cannot be overwritten with the --force flag\n")
	}

	dev, err := device.UnmarshalDevice(raw.Raw)
	if err != nil {
		return trace.Wrap(err)
	}

	// TODO(codingllama): Figure out a way to call BulkCreateDevices here?
	_, err = client.DevicesClient().CreateDevice(ctx, &devicepb.CreateDeviceRequest{
		Device:           dev,
		CreateAsResource: true,
	})
	if err != nil {
		return trail.FromGRPC(err)
	}

	fmt.Printf("Device %v/%v added to the inventory\n", dev.AssetTag, devicetrust.FriendlyOSType(dev.OsType))
	return nil
}

// Delete deletes resource by name
func (rc *ResourceCommand) Delete(ctx context.Context, client auth.ClientI) (err error) {
	singletonResources := []string{
		types.KindClusterAuthPreference,
		types.KindClusterNetworkingConfig,
		types.KindSessionRecordingConfig,
		types.KindInstaller,
		types.KindUIConfig,
	}
	if !slices.Contains(singletonResources, rc.ref.Kind) && (rc.ref.Kind == "" || rc.ref.Name == "") {
		return trace.BadParameter("provide a full resource name to delete, for example:\n$ tctl rm cluster/east\n")
	}

	switch rc.ref.Kind {
	case types.KindNode:
		if err = client.DeleteNode(ctx, apidefaults.Namespace, rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("node %v has been deleted\n", rc.ref.Name)
	case types.KindUser:
		if err = client.DeleteUser(ctx, rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("user %q has been deleted\n", rc.ref.Name)
	case types.KindRole:
		if err = client.DeleteRole(ctx, rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("role %q has been deleted\n", rc.ref.Name)
	case types.KindToken:
		if err = client.DeleteToken(ctx, rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("token %q has been deleted\n", rc.ref.Name)
	case types.KindSAMLConnector:
		if err = client.DeleteSAMLConnector(ctx, rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("SAML connector %v has been deleted\n", rc.ref.Name)
	case types.KindOIDCConnector:
		if err = client.DeleteOIDCConnector(ctx, rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("OIDC connector %v has been deleted\n", rc.ref.Name)
	case types.KindGithubConnector:
		if err = client.DeleteGithubConnector(ctx, rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("github connector %q has been deleted\n", rc.ref.Name)
	case types.KindReverseTunnel:
		if err := client.DeleteReverseTunnel(rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("reverse tunnel %v has been deleted\n", rc.ref.Name)
	case types.KindTrustedCluster:
		if err = client.DeleteTrustedCluster(ctx, rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("trusted cluster %q has been deleted\n", rc.ref.Name)
	case types.KindRemoteCluster:
		if err = client.DeleteRemoteCluster(ctx, rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("remote cluster %q has been deleted\n", rc.ref.Name)
	case types.KindSemaphore:
		if rc.ref.SubKind == "" || rc.ref.Name == "" {
			return trace.BadParameter(
				"full semaphore path must be specified (e.g. '%s/%s/alice@example.com')",
				types.KindSemaphore, types.SemaphoreKindConnection,
			)
		}
		err := client.DeleteSemaphore(ctx, types.SemaphoreFilter{
			SemaphoreKind: rc.ref.SubKind,
			SemaphoreName: rc.ref.Name,
		})
		if err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("semaphore '%s/%s' has been deleted\n", rc.ref.SubKind, rc.ref.Name)
	case types.KindClusterAuthPreference:
		if err = resetAuthPreference(ctx, client); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("cluster auth preference has been reset to defaults\n")
	case types.KindClusterNetworkingConfig:
		if err = resetClusterNetworkingConfig(ctx, client); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("cluster networking configuration has been reset to defaults\n")
	case types.KindSessionRecordingConfig:
		if err = resetSessionRecordingConfig(ctx, client); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("session recording configuration has been reset to defaults\n")
	case types.KindLock:
		name := rc.ref.Name
		if rc.ref.SubKind != "" {
			name = rc.ref.SubKind + "/" + name
		}
		if err = client.DeleteLock(ctx, name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("lock %q has been deleted\n", name)
	case types.KindDatabaseServer:
		dbServers, err := client.GetDatabaseServers(ctx, apidefaults.Namespace)
		if err != nil {
			return trace.Wrap(err)
		}
		deleted := false
		for _, server := range dbServers {
			if server.GetName() == rc.ref.Name {
				if err := client.DeleteDatabaseServer(ctx, apidefaults.Namespace, server.GetHostID(), server.GetName()); err != nil {
					return trace.Wrap(err)
				}
				deleted = true
			}
		}
		if !deleted {
			return trace.NotFound("database server %q not found", rc.ref.Name)
		}
		fmt.Printf("database server %q has been deleted\n", rc.ref.Name)
	case types.KindNetworkRestrictions:
		if err = resetNetworkRestrictions(ctx, client); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("network restrictions have been reset to defaults (allow all)\n")
	case types.KindApp:
		if err = client.DeleteApp(ctx, rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("application %q has been deleted\n", rc.ref.Name)
	case types.KindDatabase:
		if err = client.DeleteDatabase(ctx, rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("database %q has been deleted\n", rc.ref.Name)
	case types.KindKubernetesCluster:
		if err = client.DeleteKubernetesCluster(ctx, rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("kubernetes cluster %q has been deleted\n", rc.ref.Name)
	case types.KindWindowsDesktopService:
		if err = client.DeleteWindowsDesktopService(ctx, rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("windows desktop service %q has been deleted\n", rc.ref.Name)
	case types.KindWindowsDesktop:
		desktops, err := client.GetWindowsDesktops(ctx,
			types.WindowsDesktopFilter{Name: rc.ref.Name})
		if err != nil {
			return trace.Wrap(err)
		}
		if len(desktops) == 0 {
			return trace.NotFound("no desktops with name %q were found", rc.ref.Name)
		}
		deleted := 0
		var errs []error
		for _, desktop := range desktops {
			if desktop.GetName() == rc.ref.Name {
				if err = client.DeleteWindowsDesktop(ctx, desktop.GetHostID(), rc.ref.Name); err != nil {
					errs = append(errs, err)
					continue
				}
				deleted++
			}
		}
		if deleted == 0 {
			errs = append(errs,
				trace.Errorf("failed to delete any desktops with the name %q, %d were found",
					rc.ref.Name, len(desktops)))
		}
		fmts := "%d windows desktops with name %q have been deleted"
		if err := trace.NewAggregate(errs...); err != nil {
			fmt.Printf(fmts+" with errors while deleting\n", deleted, rc.ref.Name)
			return err
		}
		fmt.Printf(fmts+"\n", deleted, rc.ref.Name)
	case types.KindCertAuthority:
		if rc.ref.SubKind == "" || rc.ref.Name == "" {
			return trace.BadParameter(
				"full %s path must be specified (e.g. '%s/%s/clustername')",
				types.KindCertAuthority, types.KindCertAuthority, types.HostCA,
			)
		}
		err := client.DeleteCertAuthority(ctx, types.CertAuthID{
			Type:       types.CertAuthType(rc.ref.SubKind),
			DomainName: rc.ref.Name,
		})
		if err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("%s '%s/%s' has been deleted\n", types.KindCertAuthority, rc.ref.SubKind, rc.ref.Name)
	case types.KindKubeServer:
		kubeServers, err := client.GetKubernetesServers(ctx)
		if err != nil {
			return trace.Wrap(err)
		}
		deleted := false
		for _, server := range kubeServers {
			if server.GetName() == rc.ref.Name {
				if err := client.DeleteKubernetesServer(ctx, server.GetHostID(), server.GetName()); err != nil {
					return trace.Wrap(err)
				}
				deleted = true
			}
		}
		if !deleted {
			return trace.NotFound("kubernetes server %q not found", rc.ref.Name)
		}
		fmt.Printf("kubernetes server %q has been deleted\n", rc.ref.Name)
	case types.KindUIConfig:
		err := client.DeleteUIConfig(ctx)
		if err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("%s has been deleted\n", types.KindUIConfig)
	case types.KindInstaller:
		err := client.DeleteInstaller(ctx, rc.ref.Name)
		if err != nil {
			return trace.Wrap(err)
		}
		if rc.ref.Name == installers.InstallerScriptName {
			fmt.Printf("%s has been reset to a default value\n", rc.ref.Name)
		} else {
			fmt.Printf("%s has been deleted\n", rc.ref.Name)
		}
	case types.KindLoginRule:
		loginRuleClient := client.LoginRuleClient()
		_, err := loginRuleClient.DeleteLoginRule(ctx, &loginrulepb.DeleteLoginRuleRequest{
			Name: rc.ref.Name,
		})
		if err != nil {
			return trail.FromGRPC(err)
		}
		fmt.Printf("login rule %q has been deleted\n", rc.ref.Name)
	case types.KindSAMLIdPServiceProvider:
		if err := client.DeleteSAMLIdPServiceProvider(ctx, rc.ref.Name); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("SAML IdP service provider %q has been deleted\n", rc.ref.Name)
	case types.KindDevice:
		remote := client.DevicesClient()
		device, err := findDeviceByIDOrTag(ctx, remote, rc.ref.Name)
		if err != nil {
			return trace.Wrap(err)
		}

		if _, err := remote.DeleteDevice(ctx, &devicepb.DeleteDeviceRequest{
			DeviceId: device[0].Id,
		}); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("Device %q removed\n", rc.ref.Name)

	case types.KindAppServer:
		appServers, err := client.GetApplicationServers(ctx, rc.namespace)
		if err != nil {
			return trace.Wrap(err)
		}
		deleted := false
		for _, server := range appServers {
			if server.GetName() == rc.ref.Name {
				if err := client.DeleteApplicationServer(ctx, server.GetNamespace(), server.GetHostID(), server.GetName()); err != nil {
					return trace.Wrap(err)
				}
				deleted = true
			}
		}
		if !deleted {
			return trace.NotFound("application server %q not found", rc.ref.Name)
		}
		fmt.Printf("application server %q has been deleted\n", rc.ref.Name)
	default:
		return trace.BadParameter("deleting resources of type %q is not supported", rc.ref.Kind)
	}
	return nil
}

func resetAuthPreference(ctx context.Context, client auth.ClientI) error {
	storedAuthPref, err := client.GetAuthPreference(ctx)
	if err != nil {
		return trace.Wrap(err)
	}

	managedByStaticConfig := storedAuthPref.Origin() == types.OriginConfigFile
	if managedByStaticConfig {
		return trace.BadParameter(managedByStaticDeleteMsg)
	}

	return trace.Wrap(client.ResetAuthPreference(ctx))
}

func resetClusterNetworkingConfig(ctx context.Context, client auth.ClientI) error {
	storedNetConfig, err := client.GetClusterNetworkingConfig(ctx)
	if err != nil {
		return trace.Wrap(err)
	}

	managedByStaticConfig := storedNetConfig.Origin() == types.OriginConfigFile
	if managedByStaticConfig {
		return trace.BadParameter(managedByStaticDeleteMsg)
	}

	return trace.Wrap(client.ResetClusterNetworkingConfig(ctx))
}

func resetSessionRecordingConfig(ctx context.Context, client auth.ClientI) error {
	storedRecConfig, err := client.GetSessionRecordingConfig(ctx)
	if err != nil {
		return trace.Wrap(err)
	}

	managedByStaticConfig := storedRecConfig.Origin() == types.OriginConfigFile
	if managedByStaticConfig {
		return trace.BadParameter(managedByStaticDeleteMsg)
	}

	return trace.Wrap(client.ResetSessionRecordingConfig(ctx))
}

func resetNetworkRestrictions(ctx context.Context, client auth.ClientI) error {
	return trace.Wrap(client.DeleteNetworkRestrictions(ctx))
}

// Update updates select resource fields: expiry and labels
func (rc *ResourceCommand) Update(ctx context.Context, clt auth.ClientI) error {
	if rc.ref.Kind == "" || rc.ref.Name == "" {
		return trace.BadParameter("provide a full resource name to update, for example:\n$ tctl update rc/remote --set-labels=env=prod\n")
	}

	var err error
	var labels map[string]string
	if rc.labels != "" {
		labels, err = client.ParseLabelSpec(rc.labels)
		if err != nil {
			return trace.Wrap(err)
		}
	}

	var expiry time.Time
	if rc.ttl != "" {
		duration, err := time.ParseDuration(rc.ttl)
		if err != nil {
			return trace.Wrap(err)
		}
		expiry = time.Now().UTC().Add(duration)
	}

	if expiry.IsZero() && labels == nil {
		return trace.BadParameter("use at least one of --set-labels or --set-ttl")
	}

	switch rc.ref.Kind {
	case types.KindRemoteCluster:
		cluster, err := clt.GetRemoteCluster(rc.ref.Name)
		if err != nil {
			return trace.Wrap(err)
		}
		if labels != nil {
			meta := cluster.GetMetadata()
			meta.Labels = labels
			cluster.SetMetadata(meta)
		}
		if !expiry.IsZero() {
			cluster.SetExpiry(expiry)
		}
		if err = clt.UpdateRemoteCluster(ctx, cluster); err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("cluster %v has been updated\n", rc.ref.Name)
	default:
		return trace.BadParameter("updating resources of type %q is not supported, supported are: %q", rc.ref.Kind, types.KindRemoteCluster)
	}
	return nil
}

// IsForced returns true if -f flag was passed
func (rc *ResourceCommand) IsForced() bool {
	return rc.force
}

// getCollection lists all resources of a given type
func (rc *ResourceCommand) getCollection(ctx context.Context, client auth.ClientI) (ResourceCollection, error) {
	if rc.ref.Kind == "" {
		return nil, trace.BadParameter("specify resource to list, e.g. 'tctl get roles'")
	}

	switch rc.ref.Kind {
	case types.KindUser:
		if rc.ref.Name == "" {
			users, err := client.GetUsers(rc.withSecrets)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &userCollection{users: users}, nil
		}
		user, err := client.GetUser(rc.ref.Name, rc.withSecrets)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &userCollection{users: services.Users{user}}, nil
	case types.KindConnectors:
		sc, scErr := getSAMLConnectors(ctx, client, rc.ref.Name, rc.withSecrets)
		oc, ocErr := getOIDCConnectors(ctx, client, rc.ref.Name, rc.withSecrets)
		gc, gcErr := getGithubConnectors(ctx, client, rc.ref.Name, rc.withSecrets)
		errs := []error{scErr, ocErr, gcErr}
		allEmpty := len(sc) == 0 && len(oc) == 0 && len(gc) == 0
		reportErr := false
		for _, err := range errs {
			if err != nil && !trace.IsNotFound(err) {
				reportErr = true
				break
			}
		}
		var finalErr error
		if allEmpty || reportErr {
			finalErr = trace.NewAggregate(errs...)
		}
		return &connectorsCollection{
			saml:   sc,
			oidc:   oc,
			github: gc,
		}, finalErr
	case types.KindSAMLConnector:
		connectors, err := getSAMLConnectors(ctx, client, rc.ref.Name, rc.withSecrets)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &samlCollection{connectors}, nil
	case types.KindOIDCConnector:
		connectors, err := getOIDCConnectors(ctx, client, rc.ref.Name, rc.withSecrets)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &oidcCollection{connectors}, nil
	case types.KindGithubConnector:
		connectors, err := getGithubConnectors(ctx, client, rc.ref.Name, rc.withSecrets)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &githubCollection{connectors}, nil
	case types.KindReverseTunnel:
		if rc.ref.Name == "" {
			tunnels, err := client.GetReverseTunnels(ctx)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &reverseTunnelCollection{tunnels: tunnels}, nil
		}
		tunnel, err := client.GetReverseTunnel(rc.ref.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &reverseTunnelCollection{tunnels: []types.ReverseTunnel{tunnel}}, nil
	case types.KindCertAuthority:
		if rc.ref.SubKind == "" && rc.ref.Name == "" {
			var allAuthorities []types.CertAuthority
			for _, caType := range types.CertAuthTypes {
				authorities, err := client.GetCertAuthorities(ctx, caType, rc.withSecrets)
				if err != nil {
					if trace.IsBadParameter(err) {
						log.Warnf("failed to get certificate authority: %v; skipping", err)
						continue
					}
					return nil, trace.Wrap(err)
				}
				allAuthorities = append(allAuthorities, authorities...)
			}
			return &authorityCollection{cas: allAuthorities}, nil
		}
		id := types.CertAuthID{Type: types.CertAuthType(rc.ref.SubKind), DomainName: rc.ref.Name}
		authority, err := client.GetCertAuthority(ctx, id, rc.withSecrets)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &authorityCollection{cas: []types.CertAuthority{authority}}, nil
	case types.KindNode:
		nodes, err := client.GetNodes(ctx, rc.namespace)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if rc.ref.Name == "" {
			return &serverCollection{servers: nodes}, nil
		}
		for _, node := range nodes {
			if node.GetName() == rc.ref.Name || node.GetHostname() == rc.ref.Name {
				return &serverCollection{servers: []types.Server{node}}, nil
			}
		}
		return nil, trace.NotFound("node with ID %q not found", rc.ref.Name)
	case types.KindAuthServer:
		servers, err := client.GetAuthServers()
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if rc.ref.Name == "" {
			return &serverCollection{servers: servers}, nil
		}
		for _, server := range servers {
			if server.GetName() == rc.ref.Name || server.GetHostname() == rc.ref.Name {
				return &serverCollection{servers: []types.Server{server}}, nil
			}
		}
		return nil, trace.NotFound("auth server with ID %q not found", rc.ref.Name)
	case types.KindProxy:
		servers, err := client.GetProxies()
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if rc.ref.Name == "" {
			return &serverCollection{servers: servers}, nil
		}
		for _, server := range servers {
			if server.GetName() == rc.ref.Name || server.GetHostname() == rc.ref.Name {
				return &serverCollection{servers: []types.Server{server}}, nil
			}
		}
		return nil, trace.NotFound("proxy with ID %q not found", rc.ref.Name)
	case types.KindRole:
		if rc.ref.Name == "" {
			roles, err := client.GetRoles(ctx)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &roleCollection{roles: roles, verbose: rc.verbose}, nil
		}
		role, err := client.GetRole(ctx, rc.ref.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &roleCollection{roles: []types.Role{role}, verbose: rc.verbose}, nil
	case types.KindNamespace:
		if rc.ref.Name == "" {
			namespaces, err := client.GetNamespaces()
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &namespaceCollection{namespaces: namespaces}, nil
		}
		ns, err := client.GetNamespace(rc.ref.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &namespaceCollection{namespaces: []types.Namespace{*ns}}, nil
	case types.KindTrustedCluster:
		if rc.ref.Name == "" {
			trustedClusters, err := client.GetTrustedClusters(ctx)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &trustedClusterCollection{trustedClusters: trustedClusters}, nil
		}
		trustedCluster, err := client.GetTrustedCluster(ctx, rc.ref.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &trustedClusterCollection{trustedClusters: []types.TrustedCluster{trustedCluster}}, nil
	case types.KindRemoteCluster:
		if rc.ref.Name == "" {
			remoteClusters, err := client.GetRemoteClusters()
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &remoteClusterCollection{remoteClusters: remoteClusters}, nil
		}
		remoteCluster, err := client.GetRemoteCluster(rc.ref.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &remoteClusterCollection{remoteClusters: []types.RemoteCluster{remoteCluster}}, nil
	case types.KindSemaphore:
		sems, err := client.GetSemaphores(ctx, types.SemaphoreFilter{
			SemaphoreKind: rc.ref.SubKind,
			SemaphoreName: rc.ref.Name,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &semaphoreCollection{sems: sems}, nil
	case types.KindClusterAuthPreference:
		if rc.ref.Name != "" {
			return nil, trace.BadParameter("only simple `tctl get %v` can be used", types.KindClusterAuthPreference)
		}
		authPref, err := client.GetAuthPreference(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &authPrefCollection{authPref}, nil
	case types.KindClusterNetworkingConfig:
		if rc.ref.Name != "" {
			return nil, trace.BadParameter("only simple `tctl get %v` can be used", types.KindClusterNetworkingConfig)
		}
		netConfig, err := client.GetClusterNetworkingConfig(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &netConfigCollection{netConfig}, nil
	case types.KindSessionRecordingConfig:
		if rc.ref.Name != "" {
			return nil, trace.BadParameter("only simple `tctl get %v` can be used", types.KindSessionRecordingConfig)
		}
		recConfig, err := client.GetSessionRecordingConfig(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &recConfigCollection{recConfig}, nil
	case types.KindLock:
		if rc.ref.Name == "" {
			locks, err := client.GetLocks(ctx, false)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &lockCollection{locks: locks}, nil
		}
		name := rc.ref.Name
		if rc.ref.SubKind != "" {
			name = rc.ref.SubKind + "/" + name
		}
		lock, err := client.GetLock(ctx, name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &lockCollection{locks: []types.Lock{lock}}, nil
	case types.KindDatabaseServer:
		servers, err := client.GetDatabaseServers(ctx, rc.namespace)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if rc.ref.Name == "" {
			return &databaseServerCollection{servers: servers}, nil
		}

		var out []types.DatabaseServer
		for _, server := range servers {
			if server.GetName() == rc.ref.Name {
				out = append(out, server)
			}
		}
		if len(out) == 0 {
			return nil, trace.NotFound("database server %q not found", rc.ref.Name)
		}
		return &databaseServerCollection{servers: out}, nil
	case types.KindKubeServer:
		servers, err := client.GetKubernetesServers(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if rc.ref.Name == "" {
			return &kubeServerCollection{servers: servers}, nil
		}

		var out []types.KubeServer
		for _, server := range servers {
			if server.GetName() == rc.ref.Name || server.GetHostname() == rc.ref.Name {
				out = append(out, server)
			}
		}
		if len(out) == 0 {
			return nil, trace.NotFound("kubernetes server %q not found", rc.ref.Name)
		}
		return &kubeServerCollection{servers: out}, nil

	case types.KindAppServer:
		servers, err := client.GetApplicationServers(ctx, rc.namespace)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if rc.ref.Name == "" {
			return &appServerCollection{servers: servers}, nil
		}

		var out []types.AppServer
		for _, server := range servers {
			if server.GetName() == rc.ref.Name || server.GetHostname() == rc.ref.Name {
				out = append(out, server)
			}
		}
		if len(out) == 0 {
			return nil, trace.NotFound("application server %q not found", rc.ref.Name)
		}
		return &appServerCollection{servers: out}, nil
	case types.KindNetworkRestrictions:
		nr, err := client.GetNetworkRestrictions(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &netRestrictionsCollection{nr}, nil
	case types.KindApp:
		if rc.ref.Name == "" {
			apps, err := client.GetApps(ctx)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &appCollection{apps: apps}, nil
		}
		app, err := client.GetApp(ctx, rc.ref.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &appCollection{apps: []types.Application{app}}, nil
	case types.KindDatabase:
		if rc.ref.Name == "" {
			databases, err := client.GetDatabases(ctx)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &databaseCollection{databases: databases}, nil
		}
		database, err := client.GetDatabase(ctx, rc.ref.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &databaseCollection{databases: []types.Database{database}}, nil
	case types.KindKubernetesCluster:
		if rc.ref.Name == "" {
			clusters, err := client.GetKubernetesClusters(ctx)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &kubeClusterCollection{clusters: clusters}, nil
		}
		cluster, err := client.GetKubernetesCluster(ctx, rc.ref.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &kubeClusterCollection{clusters: []types.KubeCluster{cluster}}, nil
	case types.KindWindowsDesktopService:
		services, err := client.GetWindowsDesktopServices(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if rc.ref.Name == "" {
			return &windowsDesktopServiceCollection{services: services}, nil
		}

		var out []types.WindowsDesktopService
		for _, service := range services {
			if service.GetName() == rc.ref.Name {
				out = append(out, service)
			}
		}
		if len(out) == 0 {
			return nil, trace.NotFound("Windows desktop service %q not found", rc.ref.Name)
		}
		return &windowsDesktopServiceCollection{services: out}, nil
	case types.KindWindowsDesktop:
		desktops, err := client.GetWindowsDesktops(ctx, types.WindowsDesktopFilter{})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if rc.ref.Name == "" {
			return &windowsDesktopCollection{desktops: desktops}, nil
		}

		var out []types.WindowsDesktop
		for _, desktop := range desktops {
			if desktop.GetName() == rc.ref.Name {
				out = append(out, desktop)
			}
		}
		if len(out) == 0 {
			return nil, trace.NotFound("Windows desktop %q not found", rc.ref.Name)
		}
		return &windowsDesktopCollection{desktops: out}, nil
	case types.KindToken:
		if rc.ref.Name == "" {
			tokens, err := client.GetTokens(ctx)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &tokenCollection{tokens: tokens}, nil
		}
		token, err := client.GetToken(ctx, rc.ref.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &tokenCollection{tokens: []types.ProvisionToken{token}}, nil
	case types.KindInstaller:
		if rc.ref.Name == "" {
			installers, err := client.GetInstallers(ctx)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &installerCollection{installers: installers}, nil
		}
		inst, err := client.GetInstaller(ctx, rc.ref.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &installerCollection{installers: []types.Installer{inst}}, nil
	case types.KindUIConfig:
		if rc.ref.Name != "" {
			return nil, trace.BadParameter("only simple `tctl get %v` can be used", types.KindUIConfig)
		}
		uiconfig, err := client.GetUIConfig(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &uiConfigCollection{uiconfig}, nil
	case types.KindDatabaseService:
		resourceName := rc.ref.Name
		listReq := proto.ListResourcesRequest{
			ResourceType: types.KindDatabaseService,
		}
		if resourceName != "" {
			listReq.PredicateExpression = fmt.Sprintf(`name == "%s"`, resourceName)
		}

		getResp, err := apiclient.GetResourcesWithFilters(ctx, client, listReq)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		databaseServices, err := types.ResourcesWithLabels(getResp).AsDatabaseServices()
		if err != nil {
			return nil, trace.Wrap(err)
		}

		if len(databaseServices) == 0 && resourceName != "" {
			return nil, trace.NotFound("Database Service %q not found", resourceName)
		}

		return &databaseServiceCollection{databaseServices: databaseServices}, nil
	case types.KindLoginRule:
		loginRuleClient := client.LoginRuleClient()
		if rc.ref.Name == "" {
			fetch := func(token string) (*loginrulepb.ListLoginRulesResponse, error) {
				resp, err := loginRuleClient.ListLoginRules(ctx, &loginrulepb.ListLoginRulesRequest{
					PageToken: token,
				})
				return resp, trail.FromGRPC(err)
			}
			var rules []*loginrulepb.LoginRule
			resp, err := fetch("")
			for ; err == nil; resp, err = fetch(resp.NextPageToken) {
				rules = append(rules, resp.LoginRules...)
				if resp.NextPageToken == "" {
					break
				}
			}
			return &loginRuleCollection{rules}, trace.Wrap(err)
		}
		rule, err := loginRuleClient.GetLoginRule(ctx, &loginrulepb.GetLoginRuleRequest{
			Name: rc.ref.Name,
		})
		return &loginRuleCollection{[]*loginrulepb.LoginRule{rule}}, trail.FromGRPC(err)
	case types.KindSAMLIdPServiceProvider:
		if rc.ref.Name != "" {
			serviceProvider, err := client.GetSAMLIdPServiceProvider(ctx, rc.ref.Name)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &samlIdPServiceProviderCollection{serviceProviders: []types.SAMLIdPServiceProvider{serviceProvider}}, nil
		}
		var resources []types.SAMLIdPServiceProvider
		nextKey := ""
		for {
			var sps []types.SAMLIdPServiceProvider
			var err error
			sps, nextKey, err = client.ListSAMLIdPServiceProviders(ctx, 0, nextKey)
			if err != nil {
				return nil, trace.Wrap(err)
			}

			resources = append(resources, sps...)
			if nextKey == "" {
				break
			}
		}
		return &samlIdPServiceProviderCollection{serviceProviders: resources}, nil
	case types.KindDevice:
		remote := client.DevicesClient()
		if rc.ref.Name != "" {
			resp, err := remote.FindDevices(ctx, &devicepb.FindDevicesRequest{
				IdOrTag: rc.ref.Name,
			})
			if err != nil {
				return nil, trace.Wrap(err)
			}

			return &deviceCollection{resp.Devices}, nil
		}

		req := &devicepb.ListDevicesRequest{
			View: devicepb.DeviceView_DEVICE_VIEW_RESOURCE,
		}
		var devs []*devicepb.Device
		for {
			resp, err := remote.ListDevices(ctx, req)
			if err != nil {
				return nil, trace.Wrap(err)
			}

			devs = append(devs, resp.Devices...)

			if resp.NextPageToken == "" {
				break
			}
			req.PageToken = resp.NextPageToken
		}

		sort.Slice(devs, func(i, j int) bool {
			d1 := devs[i]
			d2 := devs[j]

			if d1.AssetTag == d2.AssetTag {
				return d1.OsType < d2.OsType
			}

			return d1.AssetTag < d2.AssetTag
		})

		return &deviceCollection{devices: devs}, nil
	}
	return nil, trace.BadParameter("getting %q is not supported", rc.ref.String())
}

func getSAMLConnectors(ctx context.Context, client auth.ClientI, name string, withSecrets bool) ([]types.SAMLConnector, error) {
	if name == "" {
		connectors, err := client.GetSAMLConnectors(ctx, withSecrets)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return connectors, nil
	}
	connector, err := client.GetSAMLConnector(ctx, name, withSecrets)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return []types.SAMLConnector{connector}, nil
}

func getOIDCConnectors(ctx context.Context, client auth.ClientI, name string, withSecrets bool) ([]types.OIDCConnector, error) {
	if name == "" {
		connectors, err := client.GetOIDCConnectors(ctx, withSecrets)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return connectors, nil
	}
	connector, err := client.GetOIDCConnector(ctx, name, withSecrets)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return []types.OIDCConnector{connector}, nil
}

func getGithubConnectors(ctx context.Context, client auth.ClientI, name string, withSecrets bool) ([]types.GithubConnector, error) {
	if name == "" {
		connectors, err := client.GetGithubConnectors(ctx, withSecrets)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return connectors, nil
	}
	connector, err := client.GetGithubConnector(ctx, name, withSecrets)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return []types.GithubConnector{connector}, nil
}

// UpsertVerb generates the correct string form of a verb based on the action taken
func UpsertVerb(exists bool, force bool) string {
	if !force && exists {
		return "updated"
	}
	return "created"
}

func checkCreateResourceWithOrigin(storedRes types.ResourceWithOrigin, resDesc string, force, confirm bool) error {
	if exists := (storedRes.Origin() != types.OriginDefaults); exists && !force {
		return trace.AlreadyExists("non-default %s already exists", resDesc)
	}
	if managedByStatic := (storedRes.Origin() == types.OriginConfigFile); managedByStatic && !confirm {
		return trace.BadParameter(`The %s resource is managed by static configuration. We recommend removing configuration from teleport.yaml, restarting the servers and trying this command again.

If you would still like to proceed, re-run the command with both --force and --confirm flags.`, resDesc)
	}
	return nil
}

const managedByStaticDeleteMsg = `This resource is managed by static configuration. In order to reset it to defaults, remove relevant configuration from teleport.yaml and restart the servers.`

func findDeviceByIDOrTag(ctx context.Context, remote devicepb.DeviceTrustServiceClient, idOrTag string) ([]*devicepb.Device, error) {
	resp, err := remote.FindDevices(ctx, &devicepb.FindDevicesRequest{
		IdOrTag: idOrTag,
	})
	switch {
	case err != nil:
		return nil, trace.Wrap(err)
	case len(resp.Devices) == 0:
		return nil, trace.NotFound("device %q not found", idOrTag)
	case len(resp.Devices) == 1:
		return resp.Devices, nil
	}

	// Do we have an ID match?
	for _, dev := range resp.Devices {
		if dev.Id == idOrTag {
			return []*devicepb.Device{dev}, nil
		}
	}

	return nil, trace.BadParameter("found multiple devices for asset tag %q, please retry using the device ID instead", idOrTag)
}
