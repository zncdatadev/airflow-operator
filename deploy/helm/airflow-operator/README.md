# Operator Chart for Apache airflow on Kubedoop

This chart bootstraps an Apache Airflow Operator deployment on a Kubernetes
cluster using the Helm package manager. It's part of the Kubedoop ecosystem.

## Installing the Chart

To install the chart with the release name `airflow-operator`:

```bash
helm install airflow-operator oci://quay.io/kubedoopcharts/airflow-operator
```

## Upgrading an Existing Release

Helm 3 does not upgrade CRDs in a chart's `crds/` directory. Before running
`helm upgrade`, validate and apply the CRD from the exact target chart version:

```bash
set -o pipefail

chart=oci://quay.io/kubedoopcharts/airflow-operator
target_version=x.y.z

helm show crds "$chart" --version "$target_version" \
  | kubectl apply --dry-run=server --validate=strict -f -
helm show crds "$chart" --version "$target_version" \
  | kubectl apply --validate=strict -f -
kubectl wait --for=condition=Established --timeout=60s \
  customresourcedefinition/airflowclusters.airflow.kubedoop.dev
```

Only after these commands succeed should the controller/chart be upgraded.
Do not add `--force-conflicts` without first understanding and resolving the
reported field ownership. The full
[operator-go v0.13 migration guide](../../../docs/operator-go-v0.13-migration.md)
describes backup, verification, compatibility, and rollback requirements.

## Usage

The operator example usage can be found in the
[examples directory](https://github.com/zncdatadev/airflow-operator/tree/main/examples).

## More information

- [Kubedoop operator for Apache airflow](https://github.com/zncdatadev/airflow-operator)
