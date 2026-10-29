//! Scratch memory for the fixed Argon2id kernel.
//!
//! Cache at most one idle arena (2 GiB at production parameters). Concurrent
//! callers still own separate arenas; surplus arenas are released on return.
//! Linux allows lazy page reclamation; Windows releases the idle arena when
//! the OS signals low available physical memory.

use blocknet_pow_kernel::PowBlock as Block;
use std::sync::{Arc, Condvar, Mutex};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

const RECLAIM_IDLE_DELAY: Duration = Duration::from_millis(50);

pub(crate) struct MemoryPool {
    blocks: usize,
    idle: Arc<Mutex<IdleMemory>>,
    wake: Arc<Condvar>,
    worker: Option<JoinHandle<()>>,
    #[cfg(target_os = "windows")]
    notifications: Option<Arc<windows::Notifications>>,
}

struct IdleMemory {
    enabled: bool,
    memory: Option<Memory>,
    reclaim_at: Option<Instant>,
    background: bool,
    stopping: bool,
    #[cfg(target_os = "windows")]
    pressure: bool,
}

impl MemoryPool {
    pub(crate) fn new(blocks: usize) -> Self {
        Self::new_inner(
            blocks,
            #[cfg(target_os = "windows")]
            windows::Notifications::new().map(Arc::new),
        )
    }

    fn new_inner(
        blocks: usize,
        #[cfg(target_os = "windows")] notifications: Option<Arc<windows::Notifications>>,
    ) -> Self {
        let idle = Arc::new(Mutex::new(IdleMemory {
            enabled: true,
            memory: None,
            reclaim_at: None,
            background: true,
            stopping: false,
            #[cfg(target_os = "windows")]
            pressure: false,
        }));
        let wake = Arc::new(Condvar::new());
        #[cfg(not(target_os = "windows"))]
        let worker = if cfg!(target_os = "linux") {
            let idle = Arc::clone(&idle);
            let wake = Arc::clone(&wake);
            thread::Builder::new()
                .name("pow-cache".into())
                .spawn(move || reclaim_idle_memory(idle, wake))
                .ok()
        } else {
            None
        };
        #[cfg(target_os = "windows")]
        let worker = notifications.as_ref().and_then(|notifications| {
            let notifications = Arc::clone(notifications);
            let idle = Arc::clone(&idle);
            let wake = Arc::clone(&wake);
            thread::Builder::new()
                .name("pow-cache".into())
                .spawn(move || windows::reclaim_idle_memory(idle, wake, notifications))
                .ok()
        });
        if worker.is_none() {
            idle.lock().expect("new cache lock").background = false;
        }
        Self {
            blocks,
            idle,
            wake,
            worker,
            #[cfg(target_os = "windows")]
            notifications,
        }
    }

    fn notify_worker(&self) {
        self.wake.notify_one();
        #[cfg(target_os = "windows")]
        if let Some(notifications) = &self.notifications {
            notifications.wake();
        }
    }

    pub(crate) fn set_cache_enabled(&self, enabled: bool) -> Result<(), ()> {
        let released = {
            let mut idle = self.idle.lock().map_err(|_| ())?;
            idle.enabled = enabled;
            if enabled {
                None
            } else {
                idle.reclaim_at = None;
                idle.memory.take()
            }
        };
        // Free an existing arena outside the cache lock. Active guards remain
        // valid and observe the new policy when they return their arenas.
        drop(released);
        self.notify_worker();
        Ok(())
    }

    pub(crate) fn checkout(&self) -> Result<MemoryGuard<'_>, ()> {
        let cached = {
            let mut idle = self.idle.lock().map_err(|_| ())?;
            idle.reclaim_at = None;
            idle.memory.take()
        };
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

impl Drop for MemoryPool {
    fn drop(&mut self) {
        if let Ok(mut idle) = self.idle.lock() {
            idle.stopping = true;
        }
        self.notify_worker();
        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
    }
}

#[cfg(not(target_os = "windows"))]
fn reclaim_idle_memory(cache: Arc<Mutex<IdleMemory>>, wake: Arc<Condvar>) {
    let Ok(mut idle) = cache.lock() else {
        return;
    };
    loop {
        if idle.stopping {
            return;
        }
        if let Some(deadline) = idle.reclaim_at {
            if let Some(wait) = deadline.checked_duration_since(Instant::now()) {
                let Ok((next, _)) = wake.wait_timeout(idle, wait) else {
                    return;
                };
                idle = next;
                continue;
            }
            // The lock excludes checkout for the entire advice operation, so
            // active hashes can never have their pages marked disposable.
            if let Some(memory) = idle.memory.as_mut() {
                if !memory.prepare_for_idle() {
                    idle.memory = None;
                }
            }
            idle.reclaim_at = None;
            wake.notify_all();
        }
        let Ok(next) = wake.wait(idle) else {
            return;
        };
        idle = next;
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
        let mut notify = false;
        if let Ok(mut idle) = self.pool.idle.lock() {
            #[cfg(target_os = "windows")]
            let can_cache = !idle.pressure;
            #[cfg(not(target_os = "windows"))]
            let can_cache = true;
            if can_cache && idle.enabled && idle.memory.is_none() {
                if let Some(memory) = self.memory.as_mut() {
                    let background = idle.background
                        && (cfg!(target_os = "windows") || !matches!(memory, Memory::Heap(_)));
                    if background || memory.prepare_for_idle() {
                        idle.memory = self.memory.take();
                        if background {
                            idle.reclaim_at = Some(Instant::now() + RECLAIM_IDLE_DELAY);
                            notify = true;
                        }
                    }
                }
            }
        }
        if notify {
            self.pool.notify_worker();
        }
        // Any surplus arena is freed after the cache lock is released.
    }
}

enum Memory {
    #[cfg(target_os = "linux")]
    Mapped(Mapping),
    Heap(Vec<Block>),
}

#[cfg(target_os = "windows")]
#[path = "pow_memory_windows.rs"]
mod windows;

impl Memory {
    fn prepare_for_idle(&mut self) -> bool {
        match self {
            #[cfg(target_os = "linux")]
            Self::Mapped(mapping) => mapping.make_reclaimable(),
            // Heap pages cannot be discarded independently of the allocator.
            // Without a native worker, release idle memory on Linux/Windows.
            Self::Heap(_) => !cfg!(any(target_os = "linux", target_os = "windows")),
        }
    }

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
    fn make_reclaimable(&mut self) -> bool {
        // SAFETY: no hash borrows this exclusively owned anonymous mapping now.
        // Every hash initializes scratch blocks before reading them, so old
        // contents are disposable. Writes cancel lazy reclamation per page;
        // subsequent reads observe the new hash's data even during pressure.
        // If the kernel cannot mark the pages reclaimable, release the arena.
        unsafe { libc::madvise(self.pointer.as_ptr().cast(), self.bytes, libc::MADV_FREE) == 0 }
    }

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

    #[cfg(any(target_os = "linux", target_os = "windows"))]
    #[test]
    fn idle_worker_marks_cached_memory_and_stops_with_pool() {
        let pool = MemoryPool::new(4096);
        assert!(pool.worker.is_some());
        let mut memory = pool.checkout().unwrap();
        let pointer = memory.as_mut().as_ptr();
        memory.as_mut()[0].as_mut()[0] = 42;
        drop(memory);
        let deadline = Instant::now() + Duration::from_secs(5);
        let mut idle = pool.idle.lock().unwrap();
        while idle.reclaim_at.is_some() {
            let remaining = deadline
                .checked_duration_since(Instant::now())
                .expect("idle worker did not reclaim the cache");
            idle = pool.wake.wait_timeout(idle, remaining).unwrap().0;
        }
        assert!(idle.memory.is_some());
        drop(idle);
        assert_eq!(pool.checkout().unwrap().as_mut().as_ptr(), pointer);
        // Pool drop wakes and joins the worker even with another timer pending.
        drop(pool);
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn reclaimed_cached_pages_preserve_hashes_and_active_arenas() {
        let pool = MemoryPool::new(4096);
        let params = Params::new(4096, 1, 1, Some(32)).unwrap();
        let reference = Argon2::new(Algorithm::Argon2id, Version::V0x13, params);
        let hasher =
            FixedArgon2id::with_kernel(4096, CpuKernel::preferred_for_verification()).unwrap();
        let mut active = pool.checkout().unwrap();
        active.as_mut()[0].as_mut()[0] = 42;

        for nonce in [0u64, 1, u64::MAX, 0] {
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
            assert_eq!(actual, expected);
            drop(memory);

            let mut idle = pool.idle.lock().unwrap();
            let Some(Memory::Mapped(mapping)) = idle.memory.as_mut() else {
                panic!("expected a cached native mapping");
            };
            // Force the zero-filled outcome of lazy reclamation without
            // depending on the test host being under memory pressure.
            let result = unsafe {
                libc::madvise(
                    mapping.pointer.as_ptr().cast(),
                    mapping.bytes,
                    libc::MADV_DONTNEED,
                )
            };
            assert_eq!(result, 0);
            assert_eq!(mapping.as_mut()[0].as_ref()[0], 0);
            assert_eq!(active.as_mut()[0].as_ref()[0], 42);
        }
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn heap_fallback_is_not_cached() {
        let pool = MemoryPool::new(32);
        drop(MemoryGuard {
            pool: &pool,
            memory: Some(Memory::heap(32).unwrap()),
        });
        assert!(pool.idle.lock().unwrap().memory.is_none());
    }

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
