test_storage_profiles() {
  # shellcheck disable=2039

  INCUS_STORAGE_DIR=$(mktemp -d -p "${TEST_DIR}" XXXXXXXXX)
  chmod +x "${INCUS_STORAGE_DIR}"
  spawn_incus "${INCUS_STORAGE_DIR}" false
  (
    set -e
    # shellcheck disable=2030
    INCUS_DIR="${INCUS_STORAGE_DIR}"

    HAS_ZFS="dir"
    if command -v zfs >/dev/null 2>&1; then
      HAS_ZFS="zfs"
    fi

    HAS_BTRFS="dir"
    if command -v btrfs >/dev/null 2>&1; then
      HAS_BTRFS="btrfs"
    fi

    # shellcheck disable=SC1009
    # Create loop file zfs pool.
    inc storage create "incustest-$(basename "${INCUS_DIR}")-pool1" "${HAS_ZFS}"

    # Create loop file btrfs pool.
    inc storage create "incustest-$(basename "${INCUS_DIR}")-pool2" "${HAS_BTRFS}"

    inc storage create "incustest-$(basename "${INCUS_DIR}")-pool4" dir

    # Set default storage pool for image import.
    inc profile device add default root disk path="/" pool="incustest-$(basename "${INCUS_DIR}")-pool1"

    # Import image into default storage pool.
    ensure_import_testimage

    inc profile create test

    # Create a new profile that provides a root device for some containers.
    inc profile device add test rootfs disk path="/" pool="incustest-$(basename "${INCUS_DIR}")-pool1"

    # Begin interesting test cases.

    for i in $(seq 1 3); do
      inc launch testimage c"${i}" --profile test
    done

    # Check that we can't remove or change the root disk device since containers
    # are actually using it.
    ! inc profile device remove test rootfs || false
    ! inc profile device set test rootfs pool "incustest-$(basename "${INCUS_DIR}")-pool2" || false

    # Give all the containers we started their own local root disk device.
    for i in $(seq 1 2); do
      ! inc config device add c"${i}" root disk path="/" pool="incustest-$(basename "${INCUS_DIR}")-pool1" || false
      inc config device add c"${i}" rootfs disk path="/" pool="incustest-$(basename "${INCUS_DIR}")-pool1"
    done

    # Try to set new pool. This should fail since there is a single container
    # that has no local root disk device.
    ! inc profile device set test rootfs pool "incustest-$(basename "${INCUS_DIR}")-pool2" || false
    # This should work since it doesn't change the pool property.
    inc profile device set test rootfs pool "incustest-$(basename "${INCUS_DIR}")-pool1"
    # Check that we can not remove the root disk device since there is a single
    # container that is still using it.
    ! inc profile device remove test rootfs || false

    # Give the last container a local root disk device.
    ! inc config device add c3 root disk path="/" pool="incustest-$(basename "${INCUS_DIR}")-pool1" || false
    inc config device add c3 rootfs disk path="/" pool="incustest-$(basename "${INCUS_DIR}")-pool1"

    # Try to set new pool. This should work since the container has a local disk
    inc profile device set test rootfs pool "incustest-$(basename "${INCUS_DIR}")-pool2"
    inc profile device set test rootfs pool "incustest-$(basename "${INCUS_DIR}")-pool1"
    # Check that we can now remove the root disk device since no container is
    # actually using it.
    inc profile device remove test rootfs

    # Add back a root device to the profile.
    ! inc profile device add test rootfs1 disk path="/" pool="incustest-$(basename "${INCUS_DIR}")-pool1" || false

    # Try to add another root device to the profile that tries to set a pool
    # property. This should fail. This is also a test for whether it is possible
    # to put multiple disk devices on the same path. This must fail!
    ! inc profile device add test rootfs2 disk path="/" pool="incustest-$(basename "${INCUS_DIR}")-pool2" || false

    # Add another root device to the profile that does not set a pool property.
    # This should not work since it would use the same path.
    ! inc profile device add test rootfs3 disk path="/" || false

    # Create a second profile.
    inc profile create testDup
    inc profile device add testDup rootfs1 disk path="/" pool="incustest-$(basename "${INCUS_DIR}")-pool1"

    # Create a third profile
    inc profile create testNoDup
    inc profile device add testNoDup rootfs2 disk path="/" pool="incustest-$(basename "${INCUS_DIR}")-pool2"

    # Verify that we cannot create a container with profiles that have
    # contradicting root devices.
    ! inc launch testimage cConflictingProfiles -p test -p testDup -p testNoDup || false

    # And that even with a local disk, a container can't have multiple root devices
    ! inc launch testimage cConflictingProfiles -s "incustest-$(basename "${INCUS_DIR}")-pool2" -p test -p testDup -p testNoDup || false

    # Check that we cannot assign conflicting profiles to a container that
    # relies on another profiles root disk device.
    inc launch testimage cOnDefault
    ! inc profile assign cOnDefault default,testDup,testNoDup || false

    # Verify that we can create a container with two profiles that specify the
    # same root disk device.
    inc launch testimage cNonConflictingProfiles -p test -p testDup

    # Try to remove the root disk device from one of the profiles.
    inc profile device remove test rootfs1

    # Try to remove the root disk device from the second profile.
    ! inc profile device remove testDup rootfs1 || false

    # Test that we can't remove the root disk device from the containers config
    # when the profile it is attached to specifies no root device.
    for i in $(seq 1 3); do
      ! inc config device remove c"${i}" root || false
      # Must fail.
      ! inc profile assign c"${i}" testDup,testNoDup || false
    done

    inc delete -f cNonConflictingProfiles
    inc delete -f cOnDefault
    for i in $(seq 1 3); do
      inc delete -f c"${i}"
    done

  )

  # shellcheck disable=SC2031,2269
  INCUS_DIR="${INCUS_DIR}"
  kill_incus "${INCUS_STORAGE_DIR}"
}
