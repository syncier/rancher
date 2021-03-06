package globaldns

import (
	"fmt"
	"github.com/rancher/norman/httperror"
	"net/http"
	"strings"
	"time"

	"github.com/rancher/norman/api/access"
	"github.com/rancher/norman/parse"
	"github.com/rancher/norman/types"
	"github.com/rancher/norman/types/convert"
	gaccess "github.com/rancher/rancher/pkg/api/customization/globalnamespaceaccess"
	"github.com/rancher/types/apis/management.cattle.io/v3"
	managementschema "github.com/rancher/types/apis/management.cattle.io/v3/schema"
	"github.com/rancher/types/client/management/v3"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	addProjectsAction    = "addProjects"
	removeProjectsAction = "removeProjects"
)

func (w *Wrapper) Formatter(apiContext *types.APIContext, resource *types.RawResource) {
	resource.AddAction(apiContext, addProjectsAction)
	resource.AddAction(apiContext, removeProjectsAction)
}

func (w *Wrapper) ActionHandler(actionName string, action *types.Action, request *types.APIContext) error {
	if err := access.ByID(request, &managementschema.Version, client.GlobalDNSType, request.ID, &client.GlobalDNS{}); err != nil {
		return err
	}
	split := strings.SplitN(request.ID, ":", 2)
	if len(split) != 2 {
		return fmt.Errorf("incorrect global DNS ID")
	}
	existingProjects := make(map[string]bool)
	gDNS, err := w.GlobalDNSes.GetNamespaced(split[0], split[1], v1.GetOptions{})
	if err != nil {
		return err
	}
	// ensure that caller is not a readonly member of globaldns, else abort
	callerID := request.Request.Header.Get(gaccess.ImpersonateUserHeader)
	metaAccessor, err := meta.Accessor(gDNS)
	if err != nil {
		return err
	}
	creatorID, ok := metaAccessor.GetAnnotations()[creatorIDAnn]
	if !ok {
		return fmt.Errorf("GlobalDNS %v has no creatorId annotation", metaAccessor.GetName())
	}
	ma := gaccess.MemberAccess{
		Users:     w.Users,
		GrbLister: w.GrbLister,
		GrLister:  w.GrLister,
	}
	accessType, err := ma.GetAccessTypeOfCaller(callerID, creatorID, gDNS.Name, gDNS.Spec.Members)
	if err != nil {
		return err
	}
	if accessType != gaccess.OwnerAccess {
		return fmt.Errorf("only owners can modify global DNS target projects")
	}

	actionInput, err := parse.ReadBody(request.Request)
	if err != nil {
		return err
	}
	inputProjects := convert.ToStringSlice(actionInput[client.UpdateGlobalDNSTargetsInputFieldProjectIDs])
	for _, p := range gDNS.Spec.ProjectNames {
		existingProjects[p] = true
	}

	switch actionName {
	case addProjectsAction:
		return w.addProjects(gDNS, request, inputProjects, existingProjects)
	case removeProjectsAction:
		return w.removeProjects(gDNS, request, existingProjects, inputProjects)
	default:
		return fmt.Errorf("bad action for global dns %v", actionName)
	}
}

func (w *Wrapper) addProjects(gDNS *v3.GlobalDNS, request *types.APIContext, inputProjects []string, existingProjects map[string]bool) error {
	if gDNS.Spec.MultiClusterAppName != "" {
		return httperror.NewAPIError(httperror.InvalidOption,
			fmt.Sprintf("cannot add projects to globaldns as targets if multiclusterappID is set %s", gDNS.Spec.MultiClusterAppName))
	}
	ma := gaccess.MemberAccess{
		Users:     w.Users,
		GrbLister: w.GrbLister,
		GrLister:  w.GrLister,
	}
	if err := ma.CheckCallerAccessToTargets(request, inputProjects, client.ProjectType, &client.Project{}); err != nil {
		return err
	}
	var projectsToAdd []string
	for _, p := range inputProjects {
		if !existingProjects[p] {
			projectsToAdd = append(projectsToAdd, p)
		}
	}
	return w.updateGDNS(gDNS, projectsToAdd, request, "addedProjects")
}

func (w *Wrapper) removeProjects(gDNS *v3.GlobalDNS, request *types.APIContext, existingProjects map[string]bool, inputProjects []string) error {
	toRemoveProjects := make(map[string]bool)
	var finalProjects []string
	for _, p := range inputProjects {
		toRemoveProjects[p] = true
	}
	for _, p := range gDNS.Spec.ProjectNames {
		if !toRemoveProjects[p] {
			finalProjects = append(finalProjects, p)
		}
	}
	return w.updateGDNS(gDNS, finalProjects, request, "removedProjects")
}

func (w Wrapper) updateGDNS(gDNS *v3.GlobalDNS, targetProjects []string, request *types.APIContext, message string) error {
	backoff := wait.Backoff{
		Duration: 100 * time.Millisecond,
		Factor:   2,
		Jitter:   0.5,
		Steps:    7,
	}

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		gDNSlatest, err := w.GlobalDNSes.GetNamespaced(gDNS.Namespace, gDNS.Name, v1.GetOptions{})
		if err != nil {
			return false, err
		}
		gDNSlatest.Spec.ProjectNames = targetProjects
		_, err = w.GlobalDNSes.Update(gDNSlatest)
		if err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})

	if err != nil {
		return err
	}
	op := map[string]interface{}{
		"message": message,
	}
	request.WriteResponse(http.StatusOK, op)
	return nil
}
