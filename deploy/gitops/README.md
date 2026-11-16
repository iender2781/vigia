# Despliegue GitOps (ejemplos para infra-vigia)

Estos ficheros son la plantilla de lo que se copia al repositorio GitOps `infra-vigia`, un directorio por
entorno. Argo CD aplica el chart `deploy/helm/vigia-contexto` con los valores de cada entorno.

| Fichero | Qué es |
| --- | --- |
| `cnpg-database.yaml` | Rol y base de datos `contexto` dentro del clúster CloudNativePG `vigia-pg` de Vigía |
| `externalsecret.yaml` | Secreto `vigia-contexto-secrets` sincronizado desde Vault |
| `argocd-application.yaml` | Aplicación de Argo CD por entorno |
| `<entorno>/values.yaml` | Valores del chart para dev, pre y pro |

El procedimiento completo está en el documento de implantación.
