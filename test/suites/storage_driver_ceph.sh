test_storage_driver_ceph() {
  # shellcheck disable=2039,3043
  local INCUS_STORAGE_DIR incus_backend

  incus_backend=$(storage_backend "$INCUS_DIR")
  if [ "$incus_backend" != "ceph" ]; then
    return
  fi

  INCUS_STORAGE_DIR=$(mktemp -d -p "${TEST_DIR}" XXXXXXXXX)
  chmod +x "${INCUS_STORAGE_DIR}"
  spawn_incus "${INCUS_STORAGE_DIR}" false

  (
    set -e
    # shellcheck disable=2030
    INCUS_DIR="${INCUS_STORAGE_DIR}"

    # shellcheck disable=SC1009
    inc storage create "incustest-$(basename "${INCUS_DIR}")-pool1" ceph volume.size=25MiB ceph.osd.pg_num=16

    # Set default storage pool for image import.
    inc profile device add default root disk path="/" pool="incustest-$(basename "${INCUS_DIR}")-pool1"

    # Import image into default storage pool.
    ensure_import_testimage

    # create osd pool
    ceph --cluster "${INCUS_CEPH_CLUSTER}" osd pool create "incustest-$(basename "${INCUS_DIR}")-existing-osd-pool" 1

    # Let Incus use an already existing osd pool.
    inc storage create "incustest-$(basename "${INCUS_DIR}")-pool2" ceph source="incustest-$(basename "${INCUS_DIR}")-existing-osd-pool" volume.size=25MiB ceph.osd.pg_num=16

    # Test that no invalid ceph storage pool configuration keys can be set.
    ! inc storage create "incustest-$(basename "${INCUS_DIR}")-invalid-ceph-pool-config" ceph lvm.thinpool_name=bla || false
    ! inc storage create "incustest-$(basename "${INCUS_DIR}")-invalid-ceph-pool-config" ceph lvm.use_thinpool=false || false
    ! inc storage create "incustest-$(basename "${INCUS_DIR}")-invalid-ceph-pool-config" ceph lvm.vg_name=bla || false

    # Test that all valid ceph storage pool configuration keys can be set.
    inc storage create "incustest-$(basename "${INCUS_DIR}")-valid-ceph-pool-config" ceph volume.block.filesystem=ext4 volume.block.mount_options=discard volume.size=25MiB ceph.rbd.clone_copy=true ceph.osd.pg_num=16
    inc storage delete "incustest-$(basename "${INCUS_DIR}")-valid-ceph-pool-config"

    # Muck around with some containers on various pools.
    inc init testimage c1pool1 -s "incustest-$(basename "${INCUS_DIR}")-pool1"
    inc list -c b c1pool1 | grep "incustest-$(basename "${INCUS_DIR}")-pool1"

    inc init testimage c2pool2 -s "incustest-$(basename "${INCUS_DIR}")-pool2"
    inc list -c b c2pool2 | grep "incustest-$(basename "${INCUS_DIR}")-pool2"

    inc launch testimage c3pool1 -s "incustest-$(basename "${INCUS_DIR}")-pool1"
    inc list -c b c3pool1 | grep "incustest-$(basename "${INCUS_DIR}")-pool1"

    inc launch testimage c4pool2 -s "incustest-$(basename "${INCUS_DIR}")-pool2"
    inc list -c b c4pool2 | grep "incustest-$(basename "${INCUS_DIR}")-pool2"

    inc storage set "incustest-$(basename "${INCUS_DIR}")-pool1" volume.block.filesystem xfs
    # xfs is unhappy with block devices < 48 MiB. It seems to calculate the
    # ag{count,size} parameters wrong and/or sets the data area too big.
    inc storage set "incustest-$(basename "${INCUS_DIR}")-pool1" volume.size 48MiB
    inc init testimage c5pool1 -s "incustest-$(basename "${INCUS_DIR}")-pool1"

    # Test whether dependency tracking is working correctly. We should be able
    # to create a container, copy it, which leads to a dependency relation
    # between the source container's storage volume and the copied container's
    # storage volume. Now, we delete the source container which will trigger a
    # rename operation and not an actual delete operation. Now we create a
    # container of the same name as the source container again, create a copy of
    # it to introduce another dependency relation. Now we delete the source
    # container again. This should work. If it doesn't it means the rename
    # operation tries to map the two source to the same name.
    inc init testimage a -s "incustest-$(basename "${INCUS_DIR}")-pool1"
    inc copy a b
    inc delete a
    inc init testimage a -s "incustest-$(basename "${INCUS_DIR}")-pool1"
    inc copy a c
    inc delete a
    inc delete b
    inc delete c

    inc storage volume create "incustest-$(basename "${INCUS_DIR}")-pool1" c1pool1
    inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool1" c1pool1 c1pool1 testDevice /opt
    ! inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool1" c1pool1 c1pool1 testDevice2 /opt || false
    inc storage volume detach "incustest-$(basename "${INCUS_DIR}")-pool1" c1pool1 c1pool1
    inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool1" custom/c1pool1 c1pool1 testDevice /opt
    ! inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool1" custom/c1pool1 c1pool1 testDevice2 /opt || false
    inc storage volume detach "incustest-$(basename "${INCUS_DIR}")-pool1" c1pool1 c1pool1

    inc storage volume create "incustest-$(basename "${INCUS_DIR}")-pool1" c2pool2
    inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool1" c2pool2 c2pool2 testDevice /opt
    ! inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool1" c2pool2 c2pool2 testDevice2 /opt || false
    inc storage volume detach "incustest-$(basename "${INCUS_DIR}")-pool1" c2pool2 c2pool2
    inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool1" custom/c2pool2 c2pool2 testDevice /opt
    ! inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool1" custom/c2pool2 c2pool2 testDevice2 /opt || false
    inc storage volume detach "incustest-$(basename "${INCUS_DIR}")-pool1" c2pool2 c2pool2

    inc storage volume create "incustest-$(basename "${INCUS_DIR}")-pool2" c3pool1
    inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool2" c3pool1 c3pool1 testDevice /opt
    ! inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool2" c3pool1 c3pool1 testDevice2 /opt || false
    inc storage volume detach "incustest-$(basename "${INCUS_DIR}")-pool2" c3pool1 c3pool1
    inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool2" c3pool1 c3pool1 testDevice /opt
    ! inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool2" c3pool1 c3pool1 testDevice2 /opt || false
    inc storage volume detach "incustest-$(basename "${INCUS_DIR}")-pool2" c3pool1 c3pool1

    inc storage volume create "incustest-$(basename "${INCUS_DIR}")-pool2" c4pool2
    inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool2" c4pool2 c4pool2 testDevice /opt
    ! inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool2" c4pool2 c4pool2 testDevice2 /opt || false
    inc storage volume detach "incustest-$(basename "${INCUS_DIR}")-pool2" c4pool2 c4pool2
    inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool2" custom/c4pool2 c4pool2 testDevice /opt
    ! inc storage volume attach "incustest-$(basename "${INCUS_DIR}")-pool2" custom/c4pool2 c4pool2 testDevice2 /opt || false
    inc storage volume detach "incustest-$(basename "${INCUS_DIR}")-pool2" c4pool2 c4pool2
    inc storage volume rename "incustest-$(basename "${INCUS_DIR}")-pool2" c4pool2 c4pool2-renamed
    inc storage volume rename "incustest-$(basename "${INCUS_DIR}")-pool2" c4pool2-renamed c4pool2

    inc delete -f c1pool1
    inc delete -f c3pool1
    inc delete -f c5pool1

    inc delete -f c4pool2
    inc delete -f c2pool2

    inc storage volume set "incustest-$(basename "${INCUS_DIR}")-pool1" c1pool1 size 500MiB
    inc storage volume unset "incustest-$(basename "${INCUS_DIR}")-pool1" c1pool1 size

    inc storage volume delete "incustest-$(basename "${INCUS_DIR}")-pool1" c1pool1
    inc storage volume delete "incustest-$(basename "${INCUS_DIR}")-pool1" c2pool2
    inc storage volume delete "incustest-$(basename "${INCUS_DIR}")-pool2" c3pool1
    inc storage volume delete "incustest-$(basename "${INCUS_DIR}")-pool2" c4pool2

    inc image delete testimage
    inc profile device remove default root
    inc storage delete "incustest-$(basename "${INCUS_DIR}")-pool1"
    inc storage delete "incustest-$(basename "${INCUS_DIR}")-pool2"
    ceph --cluster "${INCUS_CEPH_CLUSTER}" osd pool rm "incustest-$(basename "${INCUS_DIR}")-existing-osd-pool" "incustest-$(basename "${INCUS_DIR}")-existing-osd-pool"  --yes-i-really-really-mean-it
  )

  # shellcheck disable=SC2031
  kill_incus "${INCUS_STORAGE_DIR}"
}
