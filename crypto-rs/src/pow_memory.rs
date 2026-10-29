//! Scratch memory for the fixed Argon2id kernel.
//!
//! Cache at most one idle arena (2 GiB at production parameters). Concurrent
//! callers still own separate arenas; surplus arenas are released on return.

use blocknet_pow_kernel::PowBlock as Block;
use std::sync::Mutex;

pub(crate) struct MemoryPool {
    blocks: usize,
    idle: Mutex<IdleMemory>,
}

struct IdleMemory {
    enabled: bool,
    memory: Option<Memory>,
}

impl MemoryPool {
    pub(crate) fn new(blocks: usize) -> Self {
        Self {
            blocks,
            idle: Mutex::new(IdleMemory {
                enabled: true,
                memory: None,
            }),
        }
    }

    pub(crate) fn set_cache_enabled(&self, enabled: bool) -> Result<(), ()> {
        let released = {
            let mut idle = self.idle.lock().map_err(|_| ())?;
            idle.enabled = enabled;
            if enabled {
                None
            } else {
                idle.memory.take()
            }
        };
        // Free an existing arena outside the cache lock. Active guards remain
        // valid and observe the new policy when they return their arenas.
        drop(released);
        Ok(())
    }

    pub(crate) fn checkout(&self) -> Result<MemoryGuard<'_>, ()> {
        let cached = self.idle.lock().map_err(|_| ())?.memory.take();
        // Allocate outside the lock so a slow allocation does not block returns.
        let memory = match cached {
            Some(memory) => memory,
            None => Memory::new(self.blocks)?,
        };
        Ok(MemoryGuard {
            pool: self,
            memory: Some(memory),
        })
    }
}

pub(crate) struct MemoryGuard<'a> {
    pool: &'a MemoryPool,
    memory: Option<Memory>,
}

impl AsMut<[Block]> for MemoryGuard<'_> {
    fn as_mut(&mut self) -> &mut [Block] {
        self.memory.as_mut().expect("checked-out arena").as_mut()
    }
}

impl Drop for MemoryGuard<'_> {
    fn drop(&mut self) {
        if let Ok(mut idle) = self.pool.idle.lock() {
            if idle.enabled && idle.memory.is_none() {
                idle.memory = self.memory.take();
            }
        }
        // Any surplus arena is freed after the cache lock is released.
    }
}

enum Memory {
    #[cfg(target_os = "linux")]
    Mapped(Mapping),
    Heap(Vec<Block>),
}

impl Memory {
    fn new(blocks: usize) -> Result<Self, ()> {
        #[cfg(target_os = "linux")]
        if let Some(mapping) = Mapping::new(blocks) {
            return Ok(Self::Mapped(mapping));
        }
        Self::heap(blocks)
    }

    fn heap(blocks: usize) -> Result<Self, ()> {
        let mut memory = Vec::new();
        memory.try_reserve_exact(blocks).map_err(|_| ())?;
        memory.resize(blocks, Block::default());
        Ok(Self::Heap(memory))
    }
}

impl AsMut<[Block]> for Memory {
    fn as_mut(&mut self) -> &mut [Block] {
        match self {
            #[cfg(target_os = "linux")]
            Self::Mapped(mapping) => mapping.as_mut(),
            Self::Heap(memory) => memory,
        }
    }
}

#[cfg(target_os = "linux")]
struct Mapping {
    pointer: std::ptr::NonNull<Block>,
    bytes: usize,
    blocks: usize,
}

// Each mapping has exclusive ownership. Access requires &mut self, and a
// checked-out mapping can be transferred between threads through the pool.
#[cfg(target_os = "linux")]
unsafe impl Send for Mapping {}

#[cfg(target_os = "linux")]
impl Mapping {
    fn new(blocks: usize) -> Option<Self> {
        let bytes = blocks.checked_mul(std::mem::size_of::<Block>())?;
        if bytes == 0 || bytes > isize::MAX as usize {
            return None;
        }
        // SAFETY: request a fresh private, zero-filled mapping. mmap's page
        // alignment satisfies PowBlock's 64/128-byte alignment; zero words
        // are a valid Block, exactly as constructed by Block::default().
        let raw = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                bytes,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
                -1,
                0,
            )
        };
        if raw == libc::MAP_FAILED {
            return None;
        }
        let Some(pointer) = std::ptr::NonNull::new(raw.cast::<Block>()) else {
            // SAFETY: release the mapping if it cannot be represented as NonNull.
            unsafe {
                libc::munmap(raw, bytes);
            }
            return None;
        };
        // Best effort. The mapping remains usable with ordinary pages when
        // transparent huge pages are disabled or unavailable.
        unsafe {
            libc::madvise(raw, bytes, libc::MADV_HUGEPAGE);
        }
        Some(Self {
            pointer,
            bytes,
            blocks,
        })
    }
}

#[cfg(target_os = "linux")]
impl AsMut<[Block]> for Mapping {
    fn as_mut(&mut self) -> &mut [Block] {
        // SAFETY: the mapping owns blocks initialized, correctly aligned Blocks;
        // this exclusive borrow is bounded by the mapping's lifetime.
        unsafe { std::slice::from_raw_parts_mut(self.pointer.as_ptr(), self.blocks) }
    }
}

#[cfg(target_os = "linux")]
impl Drop for Mapping {
    fn drop(&mut self) {
        // SAFETY: pointer/bytes describe precisely the allocation owned here.
        unsafe {
            libc::munmap(self.pointer.as_ptr().cast(), self.bytes);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use argon2::{Algorithm, Argon2, Params, Version};
    use blocknet_pow_kernel::{CpuKernel, FixedArgon2id};

    #[test]
    fn disabled_cache_releases_idle_and_returned_arenas() {
        let pool = MemoryPool::new(32);
        let mut active = pool.checkout().unwrap();
        active.as_mut()[0].as_mut()[0] = 42;
        drop(pool.checkout().unwrap());
        assert!(pool.idle.lock().unwrap().memory.is_some());

        pool.set_cache_enabled(false).unwrap();
        assert!(pool.idle.lock().unwrap().memory.is_none());
        assert_eq!(active.as_mut()[0].as_ref()[0], 42);
        drop(active);
        drop(pool.checkout().unwrap());
        assert!(pool.idle.lock().unwrap().memory.is_none());

        pool.set_cache_enabled(true).unwrap();
        drop(pool.checkout().unwrap());
        assert!(pool.idle.lock().unwrap().memory.is_some());
    }

    #[test]
    fn cache_modes_preserve_hashes() {
        let pool = MemoryPool::new(4096);
        let params = Params::new(4096, 1, 1, Some(32)).unwrap();
        let reference = Argon2::new(Algorithm::Argon2id, Version::V0x13, params);
        let hasher =
            FixedArgon2id::with_kernel(4096, CpuKernel::preferred_for_verification()).unwrap();
        for enabled in [true, false, false, true] {
            pool.set_cache_enabled(enabled).unwrap();
            for nonce in [0u64, 1, u64::MAX] {
                let mut expected = [0u8; 32];
                let mut actual = [0u8; 32];
                reference
                    .hash_password_into(&nonce.to_le_bytes(), b"test_header", &mut expected)
                    .unwrap();
                let mut memory = pool.checkout().unwrap();
                hasher
                    .hash_password_into_with_memory(
                        &nonce.to_le_bytes(),
                        b"test_header",
                        &mut actual,
                        memory.as_mut(),
                    )
                    .unwrap();
                assert_eq!(actual, expected, "cache={enabled}, nonce={nonce}");
                drop(memory);
                assert_eq!(pool.idle.lock().unwrap().memory.is_some(), enabled);
            }
        }
    }

    #[test]
    fn reuse_and_concurrent_checkouts_keep_only_one_idle_arena() {
        let pool = MemoryPool::new(32);
        let mut a = pool.checkout().unwrap();
        let ptr = a.as_mut().as_ptr();
        let mut b = pool.checkout().unwrap();
        assert_ne!(ptr, b.as_mut().as_ptr());
        drop(a);
        drop(b);
        let mut reused = pool.checkout().unwrap();
        assert_eq!(ptr, reused.as_mut().as_ptr());
        assert!(pool.idle.lock().unwrap().memory.is_none());
        drop(reused);
        assert!(pool.idle.lock().unwrap().memory.is_some());
    }

    fn check_reference(mut memory: Memory, kib: u32) {
        let params = Params::new(kib, 1, 1, Some(32)).unwrap();
        let reference = Argon2::new(Algorithm::Argon2id, Version::V0x13, params);
        let hasher =
            FixedArgon2id::with_kernel(kib, CpuKernel::preferred_for_verification()).unwrap();
        for header in [b"12345678".as_slice(), b"different_block_header".as_slice()] {
            for nonce in [0u64, 1, u64::MAX, 0] {
                let mut expected = [0u8; 32];
                let mut actual = [0u8; 32];
                reference
                    .hash_password_into(&nonce.to_le_bytes(), header, &mut expected)
                    .unwrap();
                hasher
                    .hash_password_into_with_memory(
                        &nonce.to_le_bytes(),
                        header,
                        &mut actual,
                        memory.as_mut(),
                    )
                    .unwrap();
                assert_eq!(actual, expected);
            }
        }
    }

    #[test]
    fn reused_heap_and_native_memory_match_fresh_reference_hashes() {
        for kib in [8, 32, 4096] {
            check_reference(Memory::heap(kib as usize).unwrap(), kib);
            check_reference(Memory::new(kib as usize).unwrap(), kib);
        }
    }

    #[test]
    fn concurrent_hashes_use_separate_memory_and_match_reference() {
        let pool = MemoryPool::new(4096);
        let barrier = std::sync::Barrier::new(4);
        std::thread::scope(|scope| {
            for nonce in 0u64..4 {
                let pool = &pool;
                let barrier = &barrier;
                scope.spawn(move || {
                    let mut memory = pool.checkout().unwrap();
                    barrier.wait();
                    let params = Params::new(4096, 1, 1, Some(32)).unwrap();
                    let reference = Argon2::new(Algorithm::Argon2id, Version::V0x13, params);
                    let hasher =
                        FixedArgon2id::with_kernel(4096, CpuKernel::preferred_for_verification())
                            .unwrap();
                    let mut expected = [0u8; 32];
                    let mut actual = [0u8; 32];
                    reference
                        .hash_password_into(&nonce.to_le_bytes(), b"test_header", &mut expected)
                        .unwrap();
                    hasher
                        .hash_password_into_with_memory(
                            &nonce.to_le_bytes(),
                            b"test_header",
                            &mut actual,
                            memory.as_mut(),
                        )
                        .unwrap();
                    assert_eq!(actual, expected);
                });
            }
        });
    }
}
