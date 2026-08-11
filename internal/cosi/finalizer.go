/*
Copyright 2026 Raj Singh.

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

package cosi

// GarageProtectionFinalizer is independent from COSI's shared protection
// finalizer. The upstream controller may remove its finalizer as soon as its
// bookkeeping is complete; this one keeps the object alive until Garage-side
// cleanup has completed.
const GarageProtectionFinalizer = "garage.rajsingh.info/cosi-protection"
