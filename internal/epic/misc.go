package epic

import (
	v1 "k8s.io/api/core/v1"
)

func namespacedName(svc *v1.Service) string {
	return svc.Namespace + "/" + svc.Name
}
