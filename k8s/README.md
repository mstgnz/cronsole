# Kubernetes Deployment Guide

This directory contains Kubernetes configuration files for deploying the Cronsole Manager application. This guide will help you understand and deploy the application in a Kubernetes environment.

## Directory Structure

```
k8s/
├── configmap.yaml    # Application configuration
├── secret.yaml       # Sensitive information
├── deployment.yaml   # Application deployment
├── service.yaml      # Service configuration
├── ingress.yaml      # Ingress configuration
├── hpa.yaml         # Horizontal Pod Autoscaler
├── prometheus/         # Prometheus configuration
│   ├── prometheus-config.yaml
│   └── prometheus-deployment.yaml
└── grafana/           # Grafana configuration
    ├── grafana-deployment.yaml
    ├── grafana-datasource.yaml
    ├── grafana-secret.yaml
    └── grafana-ingress.yaml
```

## Prerequisites

- Kubernetes cluster (v1.20+)
- kubectl CLI tool
- Docker registry access
- Nginx Ingress Controller
- cert-manager (for SSL/TLS)

## Configuration Files

### 1. ConfigMap (configmap.yaml)

Non-sensitive configuration:

- application settings, database connection, timezone
- scheduler limits and watchdog thresholds
- `WATCHDOG_ALERT_TO` is **required**: production boot is refused without it,
  because the watchdog is the only thing that notices the scheduler itself
  dying, and with no recipient it notices in silence

### 2. Secret (secret.yaml)

Sensitive values, as `stringData` so they are edited in plain text:

- `JWT_SECRET` (at least 32 characters, or boot is refused)
- `DB_PASS`
- `MAIL_USER` and `MAIL_PASS`

There is no administrator credential in the secret. The first account is
created through `/setup`, which is the only screen a deployment with no
accounts serves and which closes for good once one exists. On a cluster that
means reaching it before the ingress is public, or port-forwarding to a pod and
completing it there.

The names have to match what `internal/config` reads. They did not before:
`DB_PASSWORD` and `SMTP_*` were never looked up, so the application connected
with an empty password and sent no mail, with nothing to say why.

### 3. Deployment (deployment.yaml)

- 3 replicas. **Every replica runs the dispatcher, and that is safe.**
  Exclusivity comes from two database constraints and from nothing held in a
  process: `UNIQUE (job_id, planned_minute)` on `job_runs` gives one row per
  job per minute however many dispatchers reach that minute, and the atomic
  claim (`UPDATE ... WHERE status = 'pending'`) gives one execution per row.
  All replicas must point at the same database; nothing else has to be
  coordinated.
- Rolling update strategy, resource limits and requests.
- Probes: `/healthz` touches nothing but the process, so a database hiccup does
  not restart every pod; `/readyz` checks the database, so a pod that cannot
  reach it stops taking traffic.
- `terminationGracePeriodSeconds: 120`, above the application's own 90 second
  drain. A run cut off mid-flight leaves a row in `running` that nothing closes
  until the watchdog sweeps, and a job marked `single_run` does not execute
  again until then.
- Environment from the ConfigMap and Secret above.

### 4. Service (service.yaml)

Exposes the application within the cluster:

- ClusterIP type service
- Port 80 forwarding to container port 8080

### 5. Ingress (ingress.yaml)

Configures external access:

- SSL/TLS termination
- Domain configuration
- Nginx ingress settings

### 6. HPA (hpa.yaml)

Configures automatic scaling:

- Min 3 replicas
- Max 10 replicas
- CPU and Memory based scaling

## Monitoring Setup

The application includes comprehensive monitoring capabilities using Prometheus and Grafana.

### 1. Create Monitoring Namespace

```bash
kubectl create namespace monitoring
```

### 2. Deploy Prometheus

```bash
# Deploy Prometheus configurations
kubectl apply -f k8s/prometheus/

# Verify Prometheus deployment
kubectl get pods -n monitoring -l app=prometheus
kubectl get svc -n monitoring prometheus-service
```

### 3. Deploy Grafana

```bash
# Deploy Grafana configurations
kubectl apply -f k8s/grafana/

# Verify Grafana deployment
kubectl get pods -n monitoring -l app=grafana
kubectl get svc -n monitoring grafana-service
```

### 4. Access Monitoring Dashboards

1. **Prometheus UI**
   - Access via port-forward:
     ```bash
     kubectl port-forward svc/prometheus-service 9090:9090 -n monitoring
     ```
   - Open http://localhost:9090 in your browser

2. **Grafana Dashboard**
   - Access via Ingress: https://grafana.example.com
   - Default credentials:
     - Username: admin
     - Password: admin (change after first login)

### 5. Available Metrics

The application exposes the following metrics:

1. **The dispatcher's own health.** Everything below it describes the past if
   the dispatcher is dead, which is why it is listed first.
   - `cronsole_dispatcher_minutes_lost_total`: minutes never processed. **The
     one to alert on at any increase**; nothing else reports it
   - `cronsole_dispatcher_last_tick_timestamp_seconds`: when it last ran
   - `cronsole_dispatcher_tick_duration_seconds`: approaching 60 means ticks
     are about to overlap
   - `cronsole_dispatcher_tick_failures_total`, `cronsole_dispatcher_ticks_total`
   - `cronsole_clock_drift_seconds`: application against database

2. **Executions**, labelled `project` and `job`.
   - `cronsole_runs_total{status}`: by outcome
   - `cronsole_run_duration_seconds`: histogram, buckets out to 600s
   - `cronsole_last_run_timestamp_seconds`, `cronsole_last_success_timestamp_seconds`:
     the pair is what makes "this job has not succeeded since 03:00" expressible
   - `cronsole_runs_queued_total`, `cronsole_runs_dispatched_total`,
     `cronsole_runs_skipped_total`

3. **Capacity.**
   - `cronsole_queue_depth`, `cronsole_queue_capacity`
   - `cronsole_quota_exceeded_total`: the concurrency cap refusing work
   - `cronsole_active_jobs`

There is no leader metric: this service elects no leader. Exclusivity comes
from a unique index and an atomic claim, so every replica dispatches and none
of them coordinates.

### 6. Grafana Dashboards

`grafana/grafana-datasource.yaml` carries a dashboard whose every panel queries
a metric this build exports. Panel order follows the reasoning above: dispatcher
health, then outcomes and duration, then capacity, then the jobs that have
stopped succeeding.

### 7. Alerting

The full table with the reasoning behind each threshold is in
[`docs/operations.md`](../docs/operations.md#metrics). The short version:

| alert when                                                       | why                                                    |
| ---------------------------------------------------------------- | ------------------------------------------------------ |
| `increase(cronsole_dispatcher_minutes_lost_total[1h]) > 0`       | a minute was never processed, and nothing else says so |
| `time() - cronsole_dispatcher_last_tick_timestamp_seconds > 300` | nothing is being queued                                |
| `cronsole_clock_drift_seconds > 30`                              | the minute boundary is what suffers                    |
| `time() - cronsole_last_success_timestamp_seconds > 86400`       | a job failing quietly, which a run count cannot catch  |
| `cronsole_queue_depth / cronsole_queue_capacity > 0.8`           | work is about to be refused                            |

## Deployment Steps

1. **Prepare Docker Image**

   ```bash
   # Build the image
   docker build -t your-registry.com/cronsole:latest .

   # Push to registry
   docker push your-registry.com/cronsole:latest
   ```

2. **Create Registry Secret**

   ```bash
   kubectl create secret docker-registry regcred \
     --docker-server=your-registry.com \
     --docker-username=your-username \
     --docker-password=your-password
   ```

3. **Update Configuration**
   - Modify `configmap.yaml` with your environment settings
   - Update `secret.yaml` with your encoded secrets:
     ```bash
     # Example: Encoding secrets
     echo -n "your-password" | base64
     ```
   - Update `ingress.yaml` with your domain

4. **Deploy Applications**

   ```bash
   # Apply all configurations
   kubectl apply -f k8s/

   # Or apply individually
   kubectl apply -f k8s/configmap.yaml
   kubectl apply -f k8s/secret.yaml
   kubectl apply -f k8s/deployment.yaml
   kubectl apply -f k8s/service.yaml
   kubectl apply -f k8s/ingress.yaml
   kubectl apply -f k8s/hpa.yaml
   ```

5. **Verify Deployment**

   ```bash
   # Check pods status
   kubectl get pods -l app=cronsole

   # Check service
   kubectl get svc cronsole-service

   # Check ingress
   kubectl get ingress cronsole-ingress

   # Check HPA
   kubectl get hpa cronsole-hpa
   ```

## Monitoring

1. **View Logs**

   ```bash
   # Get pod logs
   kubectl logs -l app=cronsole

   # Follow logs from all pods
   kubectl logs -f -l app=cronsole --all-containers
   ```

2. **Check Resources**

   ```bash
   # Get pod details
   kubectl describe pod -l app=cronsole

   # Check HPA status
   kubectl describe hpa cronsole-hpa
   ```

## Troubleshooting

1. **Pod Issues**

   ```bash
   # Check pod status
   kubectl get pods -l app=cronsole

   # Get pod details
   kubectl describe pod [pod-name]

   # Get pod logs
   kubectl logs [pod-name]
   ```

2. **Service Issues**

   ```bash
   # Check service endpoints
   kubectl get endpoints cronsole-service

   # Test service from another pod
   kubectl run test-pod --rm -it --image=busybox -- wget -qO- http://cronsole-service
   ```

3. **Ingress Issues**

   ```bash
   # Check ingress status
   kubectl describe ingress cronsole-ingress

   # Check ingress controller logs
   kubectl logs -n ingress-nginx -l app.kubernetes.io/name=ingress-nginx
   ```

## Scaling

- **Manual Scaling**

  ```bash
  # Scale deployment
  kubectl scale deployment cronsole-deployment --replicas=5
  ```

- **Auto Scaling**
  HPA will automatically scale based on CPU and Memory utilization:
  - Scales up when CPU or Memory > 80%
  - Scales down when CPU and Memory < 80%

## Maintenance

1. **Update Image**

   ```bash
   # Update deployment image
   kubectl set image deployment/cronsole-deployment cronsole=your-registry.com/cronsole:new-tag
   ```

2. **Rolling Restart**

   ```bash
   # Restart all pods
   kubectl rollout restart deployment cronsole-deployment
   ```

3. **Backup Configuration**
   ```bash
   # Export all resources
   kubectl get all -l app=cronsole -o yaml > backup.yaml
   ```

## Security Considerations

1. Always use secrets for sensitive information
2. Regularly rotate credentials
3. Use network policies to restrict traffic
4. Keep the Kubernetes cluster and ingress controller updated
5. Monitor pod security policies
6. Regularly scan container images for vulnerabilities

## Development vs Production

For development environments, consider:

- Reducing replica count
- Lowering resource limits
- Disabling HPA
- Using different ingress settings
- Setting debug level logs
