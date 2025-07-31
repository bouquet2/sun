# ☀ sun
> Who would win, Kubernetes or the unmatched power of the sun?

Simple and no-BS monitoring tool made for [bouquet2](https://github.com/kreatoo/bouquet2)

## Features
- Discord first
- No external dependencies
- Minimal
- Multiple replica support in-case the monitoring node goes down
- Comprehensive monitoring capabilities, including (but not limited to);
  - Pods
  - Nodes
    - Health conditions (Ready, MemoryPressure, DiskPressure)
    - CPU usage monitoring with configurable thresholds
  - Longhorn
    - Volumes
    - Replicas
    - Engines
    - Nodes
    - Jobs
  - GitOps
    - Compare deployed resources with Git repository
    - Alert on mismatches
    - Auto-fix mismatches (Unimplemented)

## Installation
Check out [the implementation on bouquet2](https://github.com/kreatoo/bouquet2/tree/main/manifests/core/sun).

### Environment Variables
- `WEBHOOK_URL`: Discord webhook URL (overrides config file)
- `POD_NAMESPACE`: Pod namespace for in-cluster detection

### Prerequisites for Longhorn Monitoring
- Longhorn must be installed in your Kubernetes cluster
- sun needs RBAC permissions to read Longhorn CRDs:
  - `volumes.longhorn.io`
  - `replicas.longhorn.io` 
  - `engines.longhorn.io`
  - `nodes.longhorn.io`
  - `backups.longhorn.io`

## License
sun is free software: you can redistribute it and/or modify it under the terms of the GNU Affero General Public License as published by the Free Software Foundation, either version 3 of the License.

sun is distributed in the hope that it will be useful, but WITHOUT ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License along with sun. If not, see https://www.gnu.org/licenses/.
